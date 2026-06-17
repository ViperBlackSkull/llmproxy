#!/usr/bin/env python3
"""MITM TLS proxy to intercept Claude Code's system prompt.

WARNING: For authorized testing and research on your own machines only.
Do not use this to intercept traffic you are not authorized to intercept.

Acts as an HTTP proxy (CONNECT method) that intercepts HTTPS traffic
to api.anthropic.com, extracts the system prompt, then forwards
everything to the real server.

Usage:
  python3 grab.py

Then in another terminal:
  HTTPS_PROXY=http://localhost:9999 NODE_TLS_REJECT_UNAUTHORIZED=0 claude -p "hello"
"""
import json, os, socket, ssl, subprocess, sys, tempfile, threading, http.server

LISTEN = ("127.0.0.1", 9999)
CA_DIR = os.path.join(os.path.expanduser("~"), ".grab-py")

def _openssl(*args):
    r = subprocess.run(["openssl"] + list(args), capture_output=True)
    if r.returncode != 0:
        raise RuntimeError(r.stderr.decode())

def ensure_ca():
    cert_path = os.path.join(CA_DIR, "ca.crt")
    key_path = os.path.join(CA_DIR, "ca.key")
    os.makedirs(CA_DIR, exist_ok=True)
    if os.path.exists(cert_path) and os.path.exists(key_path):
        return cert_path, key_path
    print(f"[*] Generating CA cert in {CA_DIR}", file=sys.stderr)
    _openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", key_path)
    _openssl("req", "-new", "-x509", "-key", key_path, "-out", cert_path,
             "-days", "3650", "-subj", "/CN=LLM Proxy CA/O=LLM Proxy")
    return cert_path, key_path

_cert_cache = {}
_cert_lock = threading.Lock()

def get_leaf_ctx(hostname, ca_cert, ca_key):
    with _cert_lock:
        if hostname in _cert_cache:
            return _cert_cache[hostname]
    tmp = tempfile.mkdtemp()
    kp = os.path.join(tmp, "leaf.key")
    cp = os.path.join(tmp, "leaf.crt")
    ep = os.path.join(tmp, "ext.cnf")
    with open(ep, "w") as f:
        f.write(f"subjectAltName=DNS:{hostname}\nextendedKeyUsage=serverAuth\n")
    _openssl("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", kp)
    _openssl("req", "-new", "-key", kp, "-out", os.path.join(tmp, "leaf.csr"),
             "-subj", f"/CN={hostname}/O=LLM Proxy")
    _openssl("x509", "-req", "-in", os.path.join(tmp, "leaf.csr"),
             "-CA", ca_cert, "-CAkey", ca_key, "-CAcreateserial",
             "-out", cp, "-days", "1", "-extfile", ep)
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(cp, kp)
    with _cert_lock:
        _cert_cache[hostname] = ctx
    return ctx

def extract_system_prompt(body_bytes):
    try:
        data = json.loads(body_bytes)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return None
    s = data.get("system")
    if s is None:
        return None
    if isinstance(s, list):
        s = "\n".join(
            b.get("text", "") if isinstance(b, dict) else str(b) for b in s
        )
    return s

def read_http_request(sock):
    """Read one complete HTTP request (headers + body) from a TLS socket."""
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = sock.recv(4096)
        if not chunk:
            return None, None, None
        data += chunk
    hdr_end = data.index(b"\r\n\r\n")
    header_block = data[:hdr_end].decode("utf-8", errors="replace")
    rest = data[hdr_end + 4:]

    # Parse content-length
    cl = 0
    first_line = header_block.split("\r\n")[0]
    for line in header_block.split("\r\n"):
        if line.lower().startswith("content-length:"):
            cl = int(line.split(":", 1)[1].strip())

    while len(rest) < cl:
        chunk = sock.recv(4096)
        if not chunk:
            break
        rest += chunk
    return first_line, header_block, rest

def relay(src, dst):
    """Simple bidirectional relay."""
    try:
        while True:
            chunk = src.recv(65536)
            if not chunk:
                break
            dst.sendall(chunk)
    except (OSError, ConnectionError):
        pass

class ProxyHandler(http.server.BaseHTTPRequestHandler):
    ca_cert = None
    ca_key = None

    def do_CONNECT(self):
        hostport = self.requestline.split()[1]
        if ":" not in hostport:
            hostport += ":443"
        hostname = hostport.split(":")[0]

        try:
            remote = socket.create_connection(hostport.split(":"), timeout=10)
        except OSError as e:
            print(f"[!] Cannot reach {hostport}: {e}", file=sys.stderr)
            self.connection.close()
            return

        self.connection.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")

        leaf_ctx = get_leaf_ctx(hostname, self.ca_cert, self.ca_key)
        try:
            tls_client = leaf_ctx.wrap_socket(self.connection, server_side=True)
        except ssl.SSLError as e:
            print(f"[!] TLS handshake with client failed: {e}", file=sys.stderr)
            remote.close()
            return

        rctx = ssl.create_default_context()
        try:
            tls_remote = rctx.wrap_socket(remote, server_hostname=hostname)
        except ssl.SSLError as e:
            print(f"[!] TLS handshake with {hostname} failed: {e}", file=sys.stderr)
            tls_client.close()
            return

        print(f"[*] MITM active for {hostname}", file=sys.stderr)

        found = False
        try:
            first_line, headers, body = read_http_request(tls_client)
            if first_line and body:
                print(f"[*] {first_line} ({len(body)} bytes)", file=sys.stderr)
                sp = extract_system_prompt(body)
                if sp:
                    sys.stdout.write(sp + "\n")
                    sys.stdout.flush()
                    found = True
                else:
                    print("[*] No system field in this request", file=sys.stderr)
                request_data = headers.encode() + b"\r\n\r\n" + body
                tls_remote.sendall(request_data)
                relay(tls_remote, tls_client)
            else:
                relay(tls_client, tls_remote)
        except Exception as e:
            print(f"[!] Error: {e}", file=sys.stderr)

        tls_client.close()
        tls_remote.close()
        if found:
            sys.exit(0)

    def log_message(self, *a):
        pass

def main():
    ca_cert, ca_key = ensure_ca()
    ProxyHandler.ca_cert = ca_cert
    ProxyHandler.ca_key = ca_key

    server = http.server.HTTPServer(LISTEN, ProxyHandler)
    print(f"[*] MITM proxy listening on {LISTEN[0]}:{LISTEN[1]}", file=sys.stderr)
    print(f"[*] CA cert: {ca_cert}", file=sys.stderr)
    print(file=sys.stderr)
    print("Run:", file=sys.stderr)
    print(f"  HTTPS_PROXY=http://{LISTEN[0]}:{LISTEN[1]} NODE_TLS_REJECT_UNAUTHORIZED=0 claude -p \"hello\"", file=sys.stderr)
    print(file=sys.stderr)
    server.serve_forever()

if __name__ == "__main__":
    main()
