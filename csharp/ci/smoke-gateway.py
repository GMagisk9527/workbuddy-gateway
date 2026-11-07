"""Exercise the production CLI and HTTP routes with isolated synthetic data."""

import argparse
import contextlib
import gzip
import http.server
import json
import os
from pathlib import Path
import socket
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid


ADMIN = "synthetic-native-admin"
LEGACY = "synthetic-native-legacy"
STREAM = (
    b'data: {"choices":[{"index":0,"delta":{"content":"native smoke"},'
    b'"finish_reason":"stop"}],"usage":{"credit":2,"prompt_tokens":10,'
    b'"completion_tokens":1}}\n\ndata: [DONE]\n\n'
)
CLIENT = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class SyntheticUpstream(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        assert self.headers.get("Authorization") == "Bearer synthetic-upstream-access"
        match self.path:
            case "/v2/chat/completions":
                assert body["model"] == "test-model" and body["stream"]
                for header in ("X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID"):
                    assert uuid.UUID(self.headers[header]).int != 0
                output, content_type = STREAM, "text/event-stream"
            case "/agenttool/v1/search":
                assert body["query"] == "synthetic query"
                output = b'{"results":[{"title":"Synthetic","site":"example.invalid","snippet":"Found"}],"usage":{"credit":0}}'
                content_type = "application/json"
            case "/agenttool/v1/webfetch":
                assert body["url"] == "https://example.invalid/"
                output, content_type = b'{"content":"# Synthetic page","usage":{"credit":0}}', "application/json"
            case _:
                self.send_error(404)
                return
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(output)))
        self.end_headers()
        self.wfile.write(output)


@contextlib.contextmanager
def upstream(directory):
    if socket.gethostbyname("www.workbuddy.cn") != "127.0.0.1":
        raise RuntimeError("The synthetic upstream hostname must resolve to loopback")
    cert, key = directory / "fixture.crt", directory / "fixture.key"
    subprocess.run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
        "-keyout", str(key), "-out", str(cert), "-subj", "/CN=www.workbuddy.cn",
        "-addext", "subjectAltName=DNS:www.workbuddy.cn",
    ], check=True, capture_output=True)
    with http.server.ThreadingHTTPServer(("127.0.0.1", 443), SyntheticUpstream) as server:
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(cert, key)
        server.socket = tls.wrap_socket(server.socket, server_side=True)
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            yield {"SSL_CERT_FILE": str(cert)}
        finally:
            server.shutdown()
            worker.join()


def write_json(path, value):
    path.write_text(json.dumps(value), encoding="utf-8")


def exercise(command, root, network, extra_env):
    account = root / "account"
    account.mkdir()
    (account / ".csharp-credential-preview").touch()
    write_json(account / "credential.json", {
        "auth": {"accessToken": "synthetic-upstream-access", "refreshToken": "synthetic-refresh",
                 "expiresAt": int(time.time()) + 3600},
        "account": {"uid": "synthetic-user", "nickname": "Synthetic"},
        "edition": "cn",
    })
    write_json(account / "state.json", {"version": 1, "disabled": False})
    manifest, keys, catalog = root / "accounts.json", root / "keys", root / "catalog.json"
    write_json(manifest, {"version": 1, "accounts": [{"id": "synthetic", "directory": "account"}]})
    write_json(catalog, {"schema": 5, "catalogs": {"cn": [
        {"id": "test-model", "maxInputTokens": 32768, "maxOutputTokens": 4096}
    ]}})

    env = {k: v for k, v in os.environ.items() if not k.startswith("WORKBUDDY_")}
    env.update(extra_env, WORKBUDDY_STANDALONE_ADMIN_KEY=ADMIN, WORKBUDDY_STANDALONE_API_KEY=LEGACY,
               WORKBUDDY_CATALOG_REFRESH_SECONDS="0", WORKBUDDY_MAINTENANCE_SECONDS="0",
               WORKBUDDY_RESOURCES_SECONDS="0", WORKBUDDY_DAILY_CHECKIN="0", WORKBUDDY_KEEPALIVE_HOURS="")
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    def request(path, token=None, body=None, status=200, decode=True, coding=None):
        headers = {"Authorization": f"Bearer {token}"} if token else {}
        data = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(body).encode()
            if coding:
                headers["Content-Encoding"] = coding
                data = gzip.compress(data) if coding == "gzip" else subprocess.check_output(["zstd", "-q", "-c"], input=data)
        req = urllib.request.Request(f"http://127.0.0.1:{port}{path}", data=data, headers=headers)
        try:
            response = CLIENT.open(req, timeout=12)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            data = response.read()
            assert response.code == status, (path, response.code, data.decode(errors="replace")[:400])
            return json.loads(data) if decode else data

    @contextlib.contextmanager
    def gateway():
        log_path = root / "gateway.log"
        with log_path.open("w+", encoding="utf-8") as log:
            process = subprocess.Popen(
                [*command, "--standalone-pool", str(manifest), str(keys), str(port), str(catalog)],
                env=env, cwd=root, stdout=log, stderr=subprocess.STDOUT,
            )
            try:
                for _ in range(120):
                    if process.poll() is not None:
                        raise RuntimeError("Gateway exited during startup")
                    try:
                        if request("/readyz")["status"] == "ready":
                            break
                    except urllib.error.URLError:
                        time.sleep(0.1)
                else:
                    raise TimeoutError("Gateway did not become ready")
                yield
            except Exception:
                log.seek(0)
                print(log.read()[-6000:])
                raise
            finally:
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=15)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.wait()
                        raise RuntimeError("Gateway did not drain on shutdown")
                if os.name != "nt" and process.returncode != 0:
                    log.seek(0)
                    raise RuntimeError(f"Gateway exit {process.returncode}\n{log.read()[-6000:]}")

    with gateway():
        assert request("/health")["status"] == "ok"
        assert b"panel-keys" in request("/ui/", decode=False)
        for asset in ("app.js", "style.css", "qrcodegen.js"):
            assert len(request(f"/ui/{asset}", decode=False)) > 100
        request("/v1/models", status=401)
        assert request("/v1/models", LEGACY)["data"][0]["id"] == "test-model"
        request("/admin/api/status", LEGACY, status=401)
        for endpoint in ("status", "accounts", "models", "stats", "performance", "tasks",
                         "rate-detail", "logs", "account-alerts", "settings", "credentials", "account-add"):
            assert isinstance(request(f"/admin/api/{endpoint}", ADMIN), dict)
        assert request("/admin/api/credential-edit?file=synthetic", ADMIN)["revision"]
        config = request("/admin/api/config-edit?acknowledged=1", ADMIN)
        assert request("/admin/api/config-edit", ADMIN, {
            **config, "acknowledged": True,
            "content": json.dumps({"models": {"blocklist": []}, "logs": {"retentionDays": 8}}),
        })["ok"]
        request("/admin/api/config-edit", ADMIN, {**config, "acknowledged": True}, status=409)
        issued = request("/admin/api/keys", ADMIN, {"action": "create", "name": "native-smoke", "quotaTotal": 100})
        token = issued["key"]
        messages = [{"role": "user", "content": "hello"}]
        count_body = {"model": "test-model", "messages": messages}
        count = request("/v1/messages/count_tokens", token, count_body)["input_tokens"]
        assert count > 0
        error = request("/v1/messages/count_tokens", token, status=405, decode=False)
        assert "仅支持 POST 请求".encode() in error
        request("/v1/responses", token, {"input": []}, status=400)
        if network:
            for coding in ("gzip", "zstd"):
                assert request("/v1/messages/count_tokens", token, count_body, coding=coding)["input_tokens"] == count
            for path, fields, terminal in (
                ("/v1/chat/completions", {"messages": messages}, b"[DONE]"),
                ("/v1/messages", {"messages": messages}, b"message_stop"),
                ("/v1/responses", {"input": "hello"}, b"response.completed"),
            ):
                for stream in (False, True):
                    output = request(path, token, {"model": "test-model", "stream": stream, **fields}, decode=False)
                    assert b"native smoke" in output and (not stream or terminal in output), path
            assert len(request("/v1/search", token, {"query": "synthetic query"})["results"]) == 1
            assert request("/v1/fetch", token, {"url": "https://example.invalid/"})["content"] == "# Synthetic page"
            assert request("/admin/api/stats", ADMIN)["summary"]["requests"] == 6
        usage = request("/v1/usage", token)
        expected_used = 12 if network else 0
        assert usage["used"] == expected_used and usage["ledger"]["pendingAttempts"] == 0

    preflight = subprocess.run([*command, "--state-check", str(manifest), str(keys)], env=env, cwd=root,
                               capture_output=True, text=True, encoding="utf-8", check=True, timeout=15)
    state = json.loads(preflight.stdout)
    assert state["ok"] and state["accounts"] == 1 and state["keys"] == 1
    assert state["ledger"]["Healthy"] and state["ledger"]["PendingAttempts"] == 0
    with gateway():
        assert request("/admin/api/log-settings", ADMIN)["retentionDays"] == 8
        assert request("/v1/usage", token)["used"] == expected_used
    print(json.dumps({"gateway": "passed", "modelRequests": 6 if network else 0,
                      "toolRequests": 2 if network else 0, "persistedSyntheticCredits": expected_used}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--no-upstream", action="store_true", help="Local managed preflight without a TLS fixture")
    parser.add_argument("command", nargs="+")
    args = parser.parse_args()
    command = [str(Path(arg).resolve()) for arg in args.command]
    with tempfile.TemporaryDirectory(prefix="workbuddy-native-") as scratch:
        root = Path(scratch)
        with (contextlib.nullcontext({}) if args.no_upstream else upstream(root)) as extra:
            exercise(command, root, not args.no_upstream, extra)
