from http.server import HTTPServer, BaseHTTPRequestHandler
import json
import os
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        l=int(self.headers.get('Content-Length',0))
        body=self.rfile.read(l).decode() if l else '{}'
        print(f"POST {self.path} body={body[:200]}")
        if "chat/completions" in self.path:
            data=json.loads(body) if body else {}
            stream=data.get("stream")
            if stream:
                self.send_response(200)
                self.send_header("Content-Type","text/event-stream")
                self.end_headers()
                self.wfile.write(b'data: {"choices":[{"delta":{"content":"Hello"}}]}\n\n')
                self.wfile.write(b'data: {"choices":[{"delta":{"content":" world"}}]}\n\n')
                self.wfile.write(b'data: {"usage":{"prompt_tokens":10,"completion_tokens":5}}\n\n')
                self.wfile.write(b'data: [DONE]\n\n')
            else:
                self.send_response(200)
                self.send_header("Content-Type","application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"id":"chatcmpl-mock","choices":[{"message":{"role":"assistant","content":"Hello from mock"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}).encode())
        elif "messages" in self.path:
            self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
            self.wfile.write(json.dumps({"id":"msg_mock","content":[{"type":"text","text":"Anthropic mock"}],"usage":{"input_tokens":10,"output_tokens":5}}).encode())
        elif "responses" in self.path:
            self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
            self.wfile.write(json.dumps({"id":"resp_mock","output":"ok","usage":{"prompt_tokens":10,"completion_tokens":5}}).encode())
        elif "embeddings" in self.path:
            self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
            self.wfile.write(json.dumps({"data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":2,"total_tokens":2}}).encode())
        elif "completions" in self.path:
            self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
            self.wfile.write(json.dumps({"choices":[{"text":"hi"}],"usage":{"prompt_tokens":3,"completion_tokens":3}}).encode())
        else:
            self.send_response(404); self.end_headers()
    def do_GET(self):
        if "models" in self.path:
            self.send_response(200); self.send_header("Content-Type","application/json"); self.end_headers()
            self.wfile.write(json.dumps({"object":"list","data":[{"id":"mock-model","object":"model","owned_by":"mock"}]}).encode())
        else:
            self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
if __name__ == "__main__":
    # MOCK_PORT=0 binds an ephemeral port; the chosen port is printed so
    # launch scripts can discover it. Default 8788 preserves old behavior.
    srv = HTTPServer(("127.0.0.1", int(os.environ.get("MOCK_PORT", "8788"))), H)
    print(f"mock_upstream listening on 127.0.0.1:{srv.server_port}", flush=True)
    srv.serve_forever()
