"""Independent stdlib implementation of v1: synthetic restriction-only policy.

No Go package, model library, network egress or database access. Pure results
make retry delivery idempotent. Runtime limits are set by the example supervisor.
"""
import datetime
import hmac
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

IDENTITY = dict(id="independent", version="1.0.0", model_version="none",
                policy_version="synthetic-v1", artifact_digest="none")
STARTED = datetime.datetime.now(datetime.timezone.utc).isoformat()
BINDING = ("contract", "job_id", "input_revision", "pipeline_revision",
           "configuration_digest", "processor", "stage")


def unique_pairs(items):
    out = {}
    for key, value in items:
        if key in out:
            raise ValueError("duplicate key")
        out[key] = value
    return out


class Handler(BaseHTTPRequestHandler):
    def setup(self):
        super().setup()
        self.connection.settimeout(2)

    def log_message(self, *_):
        pass  # Do not log source metadata or credentials.

    def send(self, status, body):
        data = json.dumps(body, allow_nan=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def auth(self):
        token = os.getenv("PROCESSOR_TOKEN", "")
        if token and not hmac.compare_digest(self.headers.get("Authorization", ""), "Bearer " + token):
            self.send(401, {"error": "unauthorized"})
            return False
        return True

    def do_GET(self):
        if not self.auth():
            return
        if self.path == "/v1/manifest":
            self.send(200, dict(contract="bitagent.processor.v1", processor=IDENTITY,
                                capabilities=["policy"], qualification="synthetic",
                                configuration_schema={"type": "object", "additionalProperties": False}))
        elif self.path == "/v1/status":
            self.send(200, dict(contract="bitagent.processor.v1", processor=IDENTITY,
                                state="ready", instance_id=STARTED, started_at=STARTED,
                                telemetry={}, attempts=None, unique_torrents=None, outcomes={}))
        else:
            self.send(404, {})

    def do_POST(self):
        if not self.auth():
            return
        try:
            size = int(self.headers["Content-Length"])
            if self.path != "/v1/process" or not 0 < size <= 262144:
                raise ValueError("request bounds")
            job = json.loads(self.rfile.read(size), object_pairs_hook=unique_pairs)
            if job["contract"] != "bitagent.processor.v1" or job["processor"] != IDENTITY or job["stage"] != "policy":
                raise ValueError("incompatible job")
            if datetime.datetime.fromisoformat(job["deadline"].replace("Z", "+00:00")) <= datetime.datetime.now(datetime.timezone.utc):
                raise ValueError("expired job")
            out = {key: job[key] for key in BINDING}
            out.update(outcome="abstained", proposal=None, reasons=["no_synthetic_exclusion"],
                       uncertainty=None, evidence=[], retry_after_seconds=0, telemetry={})
            if job["input"]["name"].startswith("Synthetic.Exclude."):
                out.update(outcome="excluded", reasons=["synthetic_exclusion"],
                           proposal={"exclusion": {"reason": "synthetic_exclusion", "scope": "publication"}})
            self.send(200, out)
        except (ValueError, KeyError, TypeError, OverflowError):
            self.send(422, {"error": "invalid_job"})


if __name__ == "__main__":
    HTTPServer((os.getenv("PROCESSOR_HOST", "127.0.0.1"), int(os.getenv("PROCESSOR_PORT", "8091"))), Handler).serve_forever()
