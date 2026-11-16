"""Validate a completed GPU smoke/benchmark without Ray or a live allocation."""
import argparse
import json
from pathlib import Path

from retail.config import load
from retail.gpu_result import validate


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('manifest')
    parser.add_argument('--config', required=True)
    args = parser.parse_args()
    print(json.dumps(validate(args.manifest, load(args.config)), indent=2))
