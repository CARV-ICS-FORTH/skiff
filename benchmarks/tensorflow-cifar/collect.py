"""Read-only scheduler evidence; keep queue/pod lifecycle outside application time."""

from datetime import datetime, timezone
import json
import subprocess


def collect(args):
    if args.backend == "slurm":
        if not args.job_id or not args.job_id.isdigit():
            raise ValueError("Supply a numeric --job-id")
        fields = "JobIDRaw,State,ExitCode,Submit,Start,End,ElapsedRaw,AllocCPUS,NodeList,AllocTRES"
        command = ["sacct", "-j", args.job_id, "-P", "-n", "--format=" + fields]
        rows = subprocess.check_output(command, text=True)
        records = [dict(zip(fields.split(","), line.split("|"))) for line in rows.splitlines()]
        return {"backend": "slurm", "records": records, "queue_seconds": [
            (datetime.fromisoformat(r["Start"]) - datetime.fromisoformat(r["Submit"])).total_seconds()
            for r in records if r["JobIDRaw"] == args.job_id and r["Start"] not in ("Unknown", "None", "")
        ]}
    if not args.name:
        raise ValueError("Supply --name matching the rendered run")
    def get(kind, selector=True):
        command = ["kubectl", "get", kind, "-n", args.namespace, "-o", "json"]
        if selector:
            command += ["-l", "cifar-run=" + args.name]
        return json.loads(subprocess.check_output(command, text=True))
    jobs, pods = get("jobs"), get("pods")
    names = {i["metadata"]["name"] for i in jobs["items"] + pods["items"]}
    events = get("events", False)
    events["items"] = [e for e in events["items"] if e.get("involvedObject", {}).get("name") in names]
    return {"backend": args.backend, "observed_utc": datetime.now(timezone.utc).isoformat(),
            "jobs": jobs, "pods": pods, "events": events,
            "note": "Pod creation/start/container timestamps are retained; they do not include the outer Slurm allocation queue."}
