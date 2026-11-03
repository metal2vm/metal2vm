#!/usr/bin/env python3
# Sends keystrokes to a VM console over VNC (RFB 3.8, no auth), for lab
# automation such as clicking through Windows setup. Use with
#   virtctl vnc --proxy-only --port 5901 <vm>
#
# Usage: vnc-type.py <port> <action>...
#   text:<string>   type a string
#   key:<name>      press a key: enter tab esc space up down left right f1..f12
#   combo:<a>+<b>   press keys together, e.g. combo:ctrl+alt+del, combo:super+r
#   sleep:<sec>     wait
import socket
import struct
import sys
import time

KEYS = {
    "enter": 0xFF0D, "tab": 0xFF09, "esc": 0xFF1B, "space": 0x0020, "backspace": 0xFF08,
    "up": 0xFF52, "down": 0xFF54, "left": 0xFF51, "right": 0xFF53,
    "shift": 0xFFE1, "ctrl": 0xFFE3, "alt": 0xFFE9, "super": 0xFFEB, "del": 0xFFFF,
    **{f"f{i}": 0xFFBD + i for i in range(1, 13)},
}
SHIFTED = '~!@#$%^&*()_+{}|:"<>?'


def recv_exact(s, n):
    buf = b""
    while len(buf) < n:
        chunk = s.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("VNC server closed the connection")
        buf += chunk
    return buf


def connect(port):
    s = socket.create_connection(("127.0.0.1", port), timeout=30)
    version = recv_exact(s, 12)
    if not version.startswith(b"RFB "):
        raise ConnectionError(f"not a VNC server: {version!r}")
    s.sendall(b"RFB 003.008\n")
    n = recv_exact(s, 1)[0]
    types = recv_exact(s, n)
    if 1 not in types:
        raise ConnectionError(f"server requires authentication {list(types)}")
    s.sendall(b"\x01")
    if struct.unpack(">I", recv_exact(s, 4))[0] != 0:
        raise ConnectionError("VNC security handshake failed")
    s.sendall(b"\x01")  # ClientInit: shared
    _, _, _ = struct.unpack(">HH16s", recv_exact(s, 20))
    name_len = struct.unpack(">I", recv_exact(s, 4))[0]
    recv_exact(s, name_len)
    s.settimeout(None)
    return s


def key(s, keysym, down):
    s.sendall(struct.pack(">BBxxI", 4, 1 if down else 0, keysym))


def press(s, *keysyms):
    for k in keysyms:
        key(s, k, True)
        time.sleep(0.03)
    for k in reversed(keysyms):
        key(s, k, False)
        time.sleep(0.03)
    time.sleep(0.08)


def type_text(s, text):
    for ch in text:
        if ch == "\n":
            press(s, KEYS["enter"])
        elif ch.isupper() or ch in SHIFTED:
            # QEMU maps the shifted keysym itself ("A", "!") to a scancode;
            # Shift + "a" would arrive as a lowercase letter.
            press(s, KEYS["shift"], ord(ch))
        else:
            press(s, ord(ch))


def main():
    port = int(sys.argv[1])
    s = connect(port)
    for action in sys.argv[2:]:
        kind, _, arg = action.partition(":")
        if kind == "text":
            type_text(s, arg)
        elif kind == "key":
            press(s, KEYS[arg])
        elif kind == "combo":
            press(s, *[KEYS[k] if k in KEYS else ord(k) for k in arg.split("+")])
        elif kind == "sleep":
            time.sleep(float(arg))
        else:
            raise SystemExit(f"unknown action {action}")
    s.close()


if __name__ == "__main__":
    main()
