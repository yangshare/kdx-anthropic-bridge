"""
mitmproxy addon: 抓 Claude Code -> 科大讯飞 /anthropic 的真实请求与响应

用法:
  mitmdump -s scripts/capture_kdx.py -p 8888

只拦截 maas-coding-api.cn-huabei-1.xf-yun.com 的流量,
请求体存 captures/req-<时间戳>.json,
响应体(含 SSE 原文)存 captures/resp-<时间戳>.txt。
"""
import json
import time
from pathlib import Path

from mitmproxy import http

TARGET_HOST = "maas-coding-api.cn-huabei-1.xf-yun.com"
CAPTURE_DIR = Path(__file__).resolve().parent.parent / "captures"
CAPTURE_DIR.mkdir(exist_ok=True)

# 记录每个请求的 stamp,供 response 关联
_stamp_map = {}


def _stamp() -> str:
    # mitmproxy 脚本里 time 可用(不是 workflow)
    return time.strftime("%Y%m%d-%H%M%S") + f"-{int((time.time() % 1) * 1000):03d}"


def request(flow: http.HTTPFlow) -> None:
    if TARGET_HOST not in flow.request.pretty_host:
        return
    stamp = _stamp()
    _stamp_map[id(flow)] = stamp

    # dump 请求体
    body = flow.request.get_text() or ""
    safe_path = "".join(c if c.isalnum() or c in "-_." else "_" for c in flow.request.path)[:40]
    req_path = CAPTURE_DIR / f"req-{stamp}_{safe_path}.json"
    try:
        parsed = json.loads(body)
        req_path.write_text(json.dumps(parsed, ensure_ascii=False, indent=2), encoding="utf-8")
    except Exception:
        req_path.write_text(body, encoding="utf-8")

    # headers 也存一份
    hdr_path = CAPTURE_DIR / f"req-{stamp}.headers.txt"
    hdr_path.write_text(
        f"{flow.request.method} {flow.request.pretty_url}\n\n"
        + "\n".join(f"{k}: {v}" for k, v in flow.request.headers.items()),
        encoding="utf-8",
    )

    print(f"[xcap] REQ {stamp} {flow.request.method} {flow.request.path} -> {req_path.name}")


def response(flow: http.HTTPFlow) -> None:
    if TARGET_HOST not in flow.request.pretty_host:
        return
    stamp = _stamp_map.pop(id(flow), None) or _stamp()

    body = flow.response.get_text() or ""
    resp_path = CAPTURE_DIR / f"resp-{stamp}.txt"
    resp_path.write_text(
        f"HTTP {flow.response.status_code}\n\n"
        + "\n".join(f"{k}: {v}" for k, v in flow.response.headers.items())
        + "\n\n"
        + body,
        encoding="utf-8",
    )
    print(f"[xcap] RESP {stamp} {flow.response.status_code} -> {resp_path.name}")
