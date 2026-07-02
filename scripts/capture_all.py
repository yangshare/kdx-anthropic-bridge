import json, time
from pathlib import Path
from mitmproxy import http
CAP = Path(__file__).resolve().parent.parent / "captures"
CAP.mkdir(exist_ok=True)
_stamp = {}
def _s():
    return time.strftime("%Y%m%d-%H%M%S") + f"-{int((time.time()%1)*1000):03d}"
def request(flow):
    h = flow.request.pretty_host
    stamp = _s()
    _stamp[id(flow)] = (stamp, h)
    body = flow.request.get_text() or ""
    safe = "".join(c if c.isalnum() or c in "-_." else "_" for c in flow.request.path)[:30]
    tag = "DS" if "deepseek" in h else ("KD" if "xf-yun" in h else "OTH")
    p = CAP / f"req-{tag}-{stamp}_{safe}.json"
    try:
        p.write_text(json.dumps(json.loads(body), ensure_ascii=False, indent=2), encoding="utf-8")
    except:
        p.write_text(body, encoding="utf-8")
    print(f"[REQ {tag}] {flow.request.method} {flow.request.path[:50]}")
def response(flow):
    stamp, h = _stamp.pop(id(flow), (_s(), flow.request.pretty_host))
    tag = "DS" if "deepseek" in h else ("KD" if "xf-yun" in h else "OTH")
    body = flow.response.get_text() or ""
    (CAP / f"resp-{tag}-{stamp}.txt").write_text(f"HTTP {flow.response.status_code}\n{flow.request.pretty_url}\n\n{body}", encoding="utf-8")
    print(f"[RESP {tag}] {flow.response.status_code}")
