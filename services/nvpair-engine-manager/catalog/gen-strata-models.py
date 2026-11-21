# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# Regenerates catalog/strata-models.json from a Strata checkout's setup.py
# tables. Run from the root of the Strata `terminal/` tree:
#   PYTHONPATH=. python3 gen-strata-models.py > strata-models.json
# Semantics mirror strata_mcp.sizes_of:
#   own-path families -> their own "models" list
#   other families    -> MODELS entries whose "families" tuple contains them
#                        (default ("qwen","swift")), non-experimental first.
import json, sys, datetime, runpy
from pathlib import Path

ns = runpy.run_path("setup.py")
FAM = dict(ns["FAMILIES"])
OWN = ns.get("OWN_PATH_FAMILIES") or {}
MODELS = ns["MODELS"]

AUTHOR = {
    "qwen": "ISTA-DASLab", "coder": "ISTA-DASLab", "unsloth": "Unsloth",
    "gyro": "agentaionai", "swift": "UkisAI", "nvfp4": "strata packs",
    "qwen36": "Unsloth",
}

def hf_url(base, q):
    u = base.format(q=q) if "{q}" in base else base
    return u.rstrip("/")

rows = []
for fam in list(FAM) + list(OWN):
    d = OWN.get(fam) or FAM[fam]
    if fam in OWN:
        sizes = list(d["models"])
        pins = d.get("sha256") or {}
        def size_bytes(q):
            pin = pins.get(d["file"].format(q=q))
            return int(pin[0]) if pin else 0
    else:
        sizes = sorted((m for m in MODELS
                        if fam in MODELS[m].get("families", ("qwen", "swift"))),
                       key=lambda m: bool(MODELS[m].get("experimental")))
        def size_bytes(q, _M=MODELS):
            gb = _M[q].get("download_gb")
            return int(gb * 1e9) if gb else 0
    for q in sizes:
        url = ""
        if fam == "nvfp4" and MODELS[q].get("repo"):
            repo = MODELS[q]["repo"]
            url = repo if repo.startswith("http") else "https://huggingface.co/" + repo
        elif d.get("hf"):
            url = hf_url(d["hf"], q)
        ident = f"{fam}/{q}"
        rows.append({
            "id": ident, "name": ident,
            "author": AUTHOR.get(fam, d.get("by", "")),
            "url": url,
            "size": size_bytes(q),
            "tags": [d["title"], q],
            "family": fam,
        })

out = {
    "scrapedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds"),
    "source": "Strata 0.2.18 setup.py FAMILIES/MODELS/OWN_PATH_FAMILIES tables",
    "count": len(rows),
    "models": rows,
}
json.dump(out, sys.stdout, indent=2)
