import json, time
from pathlib import Path
from mitmproxy import http
CAP = Path(__file__).resolve().parent.parent / "captures"
CAP.mkdir(exist_ok=True)
def _s():
    return time.strftime("%Y%m%d-%H%M%S") + f"-{int((time.time()%1)*1000):03d}"
_map = {}
def request(flow):
    stamp = _s()
    _map[id(flow)] = stamp
    body = flow.request.get_text() or ""
    p = CAP / f"req-{stamp}.json"
    try: p.write_text(json.dumps(json.loads(body),ensure_ascii=False,indent=2),encoding="utf-8")
    except: p.write_text(body,encoding="utf-8")
def response(flow):
    stamp = _map.pop(id(flow), _s())
    body = flow.response.get_text() or ""
    (CAP / f"resp-{stamp}.txt").write_text(f"HTTP {flow.response.status_code}\n\n{body}",encoding="utf-8")
