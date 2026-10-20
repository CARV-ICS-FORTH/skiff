#!/usr/bin/env python3
"""Render notebook RBAC for a validated benchmark namespace."""

import re
from pathlib import Path
import sys


if len(sys.argv) != 2:
    raise SystemExit("usage: render_rbac.py NAMESPACE")
namespace = sys.argv[1]
if len(namespace) > 63 or not re.fullmatch(
    r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?", namespace
):
    raise SystemExit("invalid Kubernetes namespace")
template = Path(__file__).with_name("notebook-rbac.yaml").read_text(encoding="utf-8")
sys.stdout.write(template.replace("__BERT_NAMESPACE__", namespace))
