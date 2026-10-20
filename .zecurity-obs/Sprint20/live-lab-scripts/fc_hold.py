#!/usr/bin/env python3
"""Fix 01 live hold: one keep-alive TCP connection to summa through the tunnel.

Sends GET / every INTERVAL seconds on the SAME socket for DURATION seconds.
Logs local port + status per request; on any error logs it and exits non-zero
(no reconnect — a reconnect would hide the failure we are testing for).
"""
import http.client, socket, sys, time, datetime

HOST, PORT = "192.168.1.38", 5173
INTERVAL, DURATION = 2.0, 40 * 60

def ts():
    return datetime.datetime.now().strftime("%H:%M:%S")

conn = http.client.HTTPConnection(HOST, PORT, timeout=10)
conn.connect()
lport = conn.sock.getsockname()[1]
print(f"{ts()} START local_port={lport}", flush=True)
start = time.time()
n = 0
while time.time() - start < DURATION:
    n += 1
    try:
        conn.request("GET", "/", headers={"Connection": "keep-alive"})
        r = conn.getresponse()
        body = r.read()
        cur = conn.sock.getsockname()[1] if conn.sock else None
        if cur != lport:
            print(f"{ts()} req={n} SOCKET CHANGED {lport}->{cur}", flush=True)
            sys.exit(2)
        if n % 30 == 0 or r.status != 200:
            print(f"{ts()} req={n} status={r.status} bytes={len(body)} port={cur}", flush=True)
    except Exception as e:
        print(f"{ts()} req={n} FAIL {type(e).__name__}: {e}", flush=True)
        sys.exit(1)
    time.sleep(INTERVAL)
print(f"{ts()} DONE req={n} local_port={lport} elapsed={int(time.time()-start)}s", flush=True)
