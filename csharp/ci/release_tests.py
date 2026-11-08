#!/usr/bin/env python3
import hashlib
import io
import json
import tarfile
import tempfile
from pathlib import Path

from release import checked_bundle_name, verify_bundle, verify_smoke


def main() -> None:
    assert checked_bundle_name("gateway.tar.gz") == "gateway.tar.gz"
    for name in ("../outside.tar.gz", "..\\outside.tar.gz", "nested/gateway.tar.gz", ""):
        try:
            checked_bundle_name(name)
        except RuntimeError:
            pass
        else:
            raise AssertionError(f"unsafe bundle name was accepted: {name!r}")
    with tempfile.TemporaryDirectory(prefix="release-test-") as root:
        directory = Path(root)
        payload = b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\0" + b"native-test"
        bundle = directory / "gateway.tar.gz"
        with tarfile.open(bundle, "w:gz") as archive:
            info = tarfile.TarInfo("HttpMigration")
            info.mode = 0o755
            info.size = len(payload)
            archive.addfile(info, io.BytesIO(payload))
        sums = directory / "SHA256SUMS"
        sums.write_text(hashlib.sha256(payload).hexdigest() + "  HttpMigration\n", encoding="utf-8")
        result = verify_bundle(bundle, sums)
        assert result["binaryBytes"] == len(payload)
        assert result["bundleBytes"] == bundle.stat().st_size
        sums.write_text("0" * 64 + "  HttpMigration\n", encoding="utf-8")
        try:
            verify_bundle(bundle, sums)
        except RuntimeError:
            pass
        else:
            raise AssertionError("checksum mismatch was accepted")
        smoke = directory / "smoke.log"
        smoke.write_text(json.dumps({"gateway":"passed","modelRequests":6,"toolRequests":2,"persistedSyntheticCredits":12}), encoding="utf-8")
        assert verify_smoke(smoke)["gateway"] == "passed"
        smoke.write_text(json.dumps({"gateway":"failed","modelRequests":6,"toolRequests":2,"persistedSyntheticCredits":12}), encoding="utf-8")
        try:
            verify_smoke(smoke)
        except RuntimeError:
            pass
        else:
            raise AssertionError("failed smoke evidence was accepted")
    print(json.dumps({"releaseVerifierChecks": 9}))


if __name__ == "__main__":
    main()
