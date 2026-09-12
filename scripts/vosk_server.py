#!/usr/bin/env python3
"""Local streaming Vosk server for the Go Asterisk adapter.

Protocol: one JSON config line, then repeated big-endian uint32 length + PCM
frames. Responses are newline-delimited JSON events. Bind to loopback by
default so caller audio never leaves this machine.
"""
import argparse
import json
import socketserver
import struct
import sys
import time
from pathlib import Path

from vosk import Model, KaldiRecognizer, SetLogLevel


class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        line = self._readline()
        if not line:
            return
        try:
            config = json.loads(line)
            sample_rate = int(config.get("sample_rate", 16000))
        except (ValueError, TypeError, json.JSONDecodeError):
            return
        grammar = config.get("grammar")

        def make_recognizer():
            if grammar:
                return KaldiRecognizer(self.server.model, sample_rate, json.dumps(grammar))
            return KaldiRecognizer(self.server.model, sample_rate)

        recognizer = make_recognizer()
        recognizer.SetWords(True)
        active = False
        silence_ms = 0
        last_partial = 0.0
        while True:
            header = self._read_exact(4)
            if not header:
                break
            length = struct.unpack(">I", header)[0]
            if length & 0x80000000:
                control_length = length & 0x7fffffff
                if control_length == 0 or control_length > 1 << 20:
                    break
                control = self._read_exact(control_length)
                if not control:
                    break
                try:
                    message = json.loads(control)
                    if message.get("type") == "grammar":
                        grammar = message.get("phrases") or None
                        recognizer = make_recognizer()
                        recognizer.SetWords(True)
                except (TypeError, ValueError, json.JSONDecodeError):
                    pass
                continue
            if length == 0 or length > 1 << 20:
                break
            audio = self._read_exact(length)
            if not audio:
                break
            rms = self._rms(audio)
            if rms >= self.server.vad_threshold:
                silence_ms = 0
                if not active:
                    active = True
                    self._send({"type": "speech_started"})
            elif active:
                silence_ms += max(1, int(len(audio) * 500 / (2 * sample_rate)))
            accepted = recognizer.AcceptWaveform(audio)
            now = time.monotonic()
            if active and now - last_partial >= 0.12:
                partial = json.loads(recognizer.PartialResult()).get("partial", "").strip()
                if partial:
                    self._send({"type": "partial", "text": partial})
                last_partial = now
            if accepted or (active and silence_ms >= self.server.endpoint_ms):
                result_data = json.loads(recognizer.FinalResult())
                result = result_data.get("text", "").strip()
                if result:
                    self._send({"type": "result", "text": result, "confidence": self._confidence(result_data)})
                recognizer = make_recognizer()
                recognizer.SetWords(True)
                active = False
                silence_ms = 0
        result_data = json.loads(recognizer.FinalResult())
        result = result_data.get("text", "").strip()
        if result:
            self._send({"type": "result", "text": result, "confidence": self._confidence(result_data)})

    def _readline(self):
        data = bytearray()
        while len(data) < 4096:
            chunk = self.request.recv(1)
            if not chunk:
                return bytes(data)
            if chunk == b"\n":
                return bytes(data)
            data.extend(chunk)
        return b""

    def _read_exact(self, size):
        data = bytearray()
        while len(data) < size:
            chunk = self.request.recv(size - len(data))
            if not chunk:
                return b""
            data.extend(chunk)
        return bytes(data)

    def _send(self, event):
        self.request.sendall((json.dumps(event, separators=(",", ":")) + "\n").encode())

    @staticmethod
    def _confidence(result):
        words = result.get("result") or []
        confidences = [float(word["conf"]) for word in words if "conf" in word]
        return round(sum(confidences) / len(confidences), 3) if confidences else 0.0

    @staticmethod
    def _rms(audio):
        if len(audio) < 2:
            return 0
        count = len(audio) // 2
        values = struct.unpack("<%dh" % count, audio[:count * 2])
        return int((sum(value * value for value in values) / count) ** 0.5)


class Server(socketserver.ThreadingMixIn, socketserver.TCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", default="models/vosk-model-small-en-us-0.15")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=9021)
    parser.add_argument("--endpoint-ms", type=int, default=500)
    parser.add_argument("--vad-threshold", type=int, default=450)
    args = parser.parse_args()
    model_path = Path(args.model)
    if not model_path.exists():
        print(f"Vosk model not found: {model_path}. Run scripts/setup_vosk.sh", file=sys.stderr)
        return 2
    SetLogLevel(-1)
    model = Model(str(model_path))
    with Server((args.host, args.port), Handler) as server:
        server.model = model
        server.endpoint_ms = args.endpoint_ms
        server.vad_threshold = args.vad_threshold
        print(f"Local Vosk STT listening on {args.host}:{args.port} using {model_path}", flush=True)
        server.serve_forever()


if __name__ == "__main__":
    raise SystemExit(main())
