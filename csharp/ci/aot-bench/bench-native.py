"""Compare two complete gateway executables against the same loopback HTTPS upstream."""

import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import hashlib
import http.server
import json
import os
from pathlib import Path
import socket
import ssl
import statistics
import subprocess
import tempfile
import threading
import time

import helpers


HOST = "www.workbuddy.cn"
PORT = 18081
DELTA = (b'data: {"id":"synthetic-output","model":"bench-model","choices":'
         b'[{"index":0,"delta":{"content":"0123456789abcdef0123456789abcdef"}}]}\n\n')
TAIL = (b'data: {"id":"synthetic-output","model":"bench-model","choices":[{"index":0,"delta":{},'
        b'"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":8,'
        b'"total_tokens":108,"credit":0}}\n\ndata: [DONE]\n\n')
CASES = (
    ("messages", 1024, 1, 64),
    ("messages", 1 << 20, 1, 64),
    ("messages", 1 << 20, 4, 64),
    ("messages", 4 << 20, 1, 64),
    ("chat", 1 << 20, 4, 64),
    ("responses", 1 << 20, 4, 64),
    ("messages", 1024, 4, 1024),
)


class Replay(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        while length:
            block = self.rfile.read(min(length, 65536))
            if not block:
                return
            length -= len(block)
        if self.path != "/v2/chat/completions":
            self.send_error(404)
            return
        with self.server.guard:
            self.server.calls += 1
            output = self.server.output
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(output)))
        self.end_headers()
        try:
            self.wfile.write(DELTA)
            self.wfile.write(memoryview(output)[len(DELTA):])
        except (BrokenPipeError, ConnectionResetError):
            pass


@contextmanager
def upstream(root):
    assert socket.gethostbyname(HOST) == "127.0.0.1"
    cert, key = root / "fixture.crt", root / "fixture.key"
    subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                    "-keyout", str(key), "-out", str(cert), "-subj", f"/CN={HOST}",
                    "-addext", f"subjectAltName=DNS:{HOST}"], check=True, capture_output=True)
    with http.server.ThreadingHTTPServer(("127.0.0.1", 443), Replay) as server:
        server.guard = threading.Lock()
        server.calls = 0
        server.output = DELTA * 8 + TAIL
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield server, cert
        finally:
            server.shutdown()
            thread.join()


def write_json(path, document):
    path.write_text(json.dumps(document), encoding="utf-8")


def fixture(root, mode):
    accounts = []
    for i in range(4):
        credential = {"auth": {"accessToken": "synthetic-access", "refreshToken": "synthetic-refresh",
                               "expiresAt": int(time.time()) + 86400},
                      "account": {"uid": f"synthetic-{i}", "nickname": "Synthetic"}, "edition": "cn"}
        if mode == "go":
            write_json(root / f"workbuddy{i}.json", credential)
            continue
        identifier = f"synthetic-{i}"
        account = root / identifier
        account.mkdir()
        (account / ".csharp-credential-preview").touch()
        write_json(account / "credential.json", credential)
        write_json(account / "state.json", {"version": 1, "disabled": False})
        accounts.append({"id": identifier, "directory": identifier})
    if mode == "go":
        write_json(root / "config.json", {
            "gateway": {"apiKeyEnabled": True, "apiKey": helpers.KEY},
            "concurrent": {"MaxAcc": 4},
            "admission": {"maxActive": 8, "maxPreparing": 2},
            "credits": {"fetch": False},
            "upstream": {"requestProfile": "legacy"},
        })
    else:
        write_json(root / "accounts.json", {"version": 1, "accounts": accounts})
        write_json(root / "catalog.json", {"schema": 5, "catalogs": {"cn": [
            {"id": "bench-model", "maxInputTokens": 2097152, "maxOutputTokens": 4096},
        ]}})
        console = root / "keys" / "console"
        console.mkdir(parents=True)
        (console / ".csharp-console").write_text("console-v1")
        write_json(console / "config.json", {"admission": {"maxActive": 8, "maxPreparing": 2}})


def measure(mode, binary, case, server, env, directory, repeats, count):
    protocol, size, concurrency, frames = case
    fixture(directory, mode)
    args = [str(binary)]
    if mode == "go":
        args += ["serve", "-addr", "127.0.0.1", "-port", str(PORT), "-reload-interval", "0",
                 "-models-refresh", "0", "-keepalive-hours=", "-disable-price-probes"]
    else:
        args += ["--standalone-pool", str(directory / "accounts.json"), str(directory / "keys"),
                 str(PORT), str(directory / "catalog.json")]
    with server.guard:
        server.output = DELTA * 8 + TAIL
    assert helpers.available() > 256 * 1024 * 1024, "Insufficient memory to begin measurement"
    stop = threading.Event()
    guard = threading.Event()
    peak = {"rss": 0, "available": helpers.available()}
    with (directory / "gateway.log").open("wb") as log:
        started = time.perf_counter()
        child = subprocess.Popen(args, cwd=directory, env=env, stdout=log, stderr=log)

        def monitor():
            while not stop.wait(0.02):
                peak["rss"] = max(peak["rss"], helpers.proc(child.pid)["rss"])
                peak["available"] = min(peak["available"], helpers.available())
                if peak["rss"] > 420 * (1 << 20) or peak["available"] < 160 * (1 << 20):
                    guard.set()
                    child.terminate()
                    return

        watcher = threading.Thread(target=monitor, daemon=True)
        watcher.start()
        try:
            helpers.wait_ready(child, PORT)
            startup = 1000 * (time.perf_counter() - started)
            idle_rss = helpers.proc(child.pid)["rss"]
            warm = helpers.make_body(protocol, 1024)
            for _ in range(24):
                sample = helpers.request(PORT, protocol, warm, 8)
                assert sample["ok"], (mode, protocol, "warmup", sample)
            with server.guard:
                server.output = DELTA * frames + TAIL
            body = helpers.make_body(protocol, size)
            for _ in range(4):
                sample = helpers.request(PORT, protocol, body, frames)
                assert sample["ok"], (mode, protocol, "case warmup", sample)
            before = helpers.proc(child.pid)
            with server.guard:
                calls = server.calls
            barrier = threading.Barrier(concurrency)
            started = time.perf_counter()
            with ThreadPoolExecutor(max_workers=concurrency) as workers:
                answers = list(workers.map(lambda _: helpers.request(PORT, protocol, body, frames, barrier=barrier), range(count)))
            elapsed = time.perf_counter() - started
            after = helpers.proc(child.pid)
            with server.guard:
                assert server.calls - calls == count
            failures = [sample for sample in answers if not sample["ok"]]
            assert not guard.is_set() and not failures, (mode, case, "failed", failures[:2])
            return {"mode": mode, "protocol": protocol, "targetBytes": size, "wireBytes": len(body),
                    "concurrency": concurrency, "frames": frames, "round": repeats, "requests": count,
                    "startupMs": startup, "idleRss": idle_rss, "peakRss": max(peak["rss"], after["hwm"]),
                    "minAvailable": peak["available"], "elapsed": elapsed,
                    "cpuSeconds": after["cpu_s"] - before["cpu_s"], "answers": answers}
        finally:
            stop.set()
            watcher.join(timeout=1)
            helpers.stop(child)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("go", type=Path)
    parser.add_argument("aot", type=Path)
    parser.add_argument("out", type=Path)
    parser.add_argument("--rounds", type=int, default=3, choices=(1, 3))
    parser.add_argument("--requests", type=int, default=16, choices=(4, 16))
    args = parser.parse_args()
    args.out.mkdir(exist_ok=False)
    assert {name for _, name in socket.if_nameindex()} == {"lo"}, "Isolated loopback network is required"
    os.nice(10)
    modes = {"go": args.go.resolve(), "aot": args.aot.resolve()}
    env = {name: os.environ[name] for name in ("PATH", "LANG", "TZ") if name in os.environ}
    env.update(GOMEMLIMIT="512MiB", GOGC="100", GOMAXPROCS="1", DOTNET_PROCESSOR_COUNT="1",
               DOTNET_GCHeapHardLimit="0x20000000", DOTNET_EnableDiagnostics="0",
               WORKBUDDY_STANDALONE_API_KEY=helpers.KEY, WORKBUDDY_STANDALONE_ADMIN_KEY="synthetic-admin",
               WORKBUDDY_MAX_ACTIVE="8", WORKBUDDY_CATALOG_REFRESH_SECONDS="0",
               WORKBUDDY_MAINTENANCE_SECONDS="0", WORKBUDDY_RESOURCES_SECONDS="0",
               WORKBUDDY_DAILY_CHECKIN="0", WORKBUDDY_KEEPALIVE_HOURS="")
    write_json(args.out / "metadata.json", {"binaries": {
        name: {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(), "bytes": path.stat().st_size}
        for name, path in modes.items()}, "cpuAffinity": sorted(os.sched_getaffinity(0)),
        "rounds": args.rounds, "requestsPerRound": args.requests, "fixture": "loopback HTTPS/HTTP1.1, fixed SSE",
        "realModelCalls": 0})
    rows = []
    with tempfile.TemporaryDirectory(prefix="gateway-benchmark-") as tmp, (args.out / "results.jsonl").open("w") as output:
        root = Path(tmp)
        with upstream(root) as (server, cert):
            env["SSL_CERT_FILE"] = str(cert)
            for index, case in enumerate(CASES):
                for repetition in range(args.rounds):
                    order = list(modes) if (index + repetition) % 2 == 0 else list(reversed(modes))
                    for mode in order:
                        directory = args.out / f"{index}-{repetition}-{mode}"
                        directory.mkdir()
                        result = measure(mode, modes[mode], case, server, env, directory.resolve(), repetition, args.requests)
                        rows.append(result)
                        output.write(json.dumps(result) + "\n")
                        output.flush()
                        print(f"{mode} {case}: {len(result['answers'])} complete", flush=True)
    summary = []
    for protocol, size, concurrency, frames in CASES:
        row = {"protocol": protocol, "targetKiB": size // 1024, "concurrency": concurrency, "frames": frames}
        for mode in modes:
            selected = [r for r in rows if (r["mode"], r["protocol"], r["targetBytes"], r["concurrency"], r["frames"]) == (mode, protocol, size, concurrency, frames)]
            samples = [sample for r in selected for sample in r["answers"]]
            row[mode] = {"firstMs": helpers.percentile([s["ttft_ms"] for s in samples], .5),
                         "firstP95Ms": helpers.percentile([s["ttft_ms"] for s in samples], .95),
                         "completeMs": helpers.percentile([s["total_ms"] for s in samples], .5),
                         "requestsPerSecond": round(statistics.median(r["requests"] / r["elapsed"] for r in selected), 2),
                         "idleMiB": round(statistics.median(r["idleRss"] for r in selected) / (1 << 20), 2),
                         "peakMiB": round(max(r["peakRss"] for r in selected) / (1 << 20), 2),
                         "cpuMsPerRequest": round(1000 * sum(r["cpuSeconds"] for r in selected) / len(samples), 2)}
        summary.append(row)
    write_json(args.out / "summary.json", {"results": summary, "completed": sum(r["requests"] for r in rows)})


if __name__ == "__main__":
    main()
