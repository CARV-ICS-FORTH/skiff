"""The same CPU/GPU TensorFlow application runs under every deployment adapter."""

import hashlib
import importlib.metadata
import os
from pathlib import Path
import platform
import socket
import threading
import time
import traceback

from common import (
    artifacts_under, checked_artifacts, config_from_args, read_json, source_identity,
    training_steps, validate_workers, wait_json, write_json,
)
from data import epoch_ids, load_data


def build_model(tf, width):
    inputs = tf.keras.Input((32, 32, 3))
    x = tf.keras.layers.Conv2D(width, 3, padding="same", activation="relu")(inputs)
    # No BatchNorm: local batch statistics would change the model when scaling.
    for stage, channels in enumerate((width, width * 2, width * 4)):
        stride = 1 if stage == 0 else 2
        shortcut = x
        if stride != 1 or x.shape[-1] != channels:
            shortcut = tf.keras.layers.Conv2D(channels, 1, strides=stride)(x)
        y = tf.keras.layers.Conv2D(channels, 3, strides=stride, padding="same", activation="relu")(x)
        y = tf.keras.layers.Conv2D(channels, 3, padding="same")(y)
        x = tf.keras.layers.ReLU()(tf.keras.layers.Add()([shortcut, y]))
    x = tf.keras.layers.GlobalAveragePooling2D()(x)
    return tf.keras.Model(inputs, tf.keras.layers.Dense(10)(x))


def state_digest(model, optimizer):
    import numpy as np

    h = hashlib.sha256()
    for variable in [*model.weights, *optimizer.variables]:
        array = np.asarray(variable.numpy())
        h.update(str((array.shape, str(array.dtype))).encode())
        h.update(array.tobytes())
    return h.hexdigest()


class ResourceSampler:
    def __init__(self):
        import psutil

        self.process = psutil.Process()
        self.started = time.monotonic()
        self.cpu_start = self.process.cpu_times()
        self.peak_rss = 0
        self.peak_threads = 0
        self.samples = 0
        self.stop = threading.Event()
        self.thread = threading.Thread(target=self.sample, daemon=True)
        self.thread.start()

    def sample(self):
        while not self.stop.is_set():
            self.peak_rss = max(self.peak_rss, self.process.memory_info().rss)
            self.peak_threads = max(self.peak_threads, self.process.num_threads())
            self.samples += 1
            self.stop.wait(0.2)

    def finish(self):
        self.stop.set()
        self.thread.join()
        cpu = self.process.cpu_times()
        return {
            "sampled_peak_rss_bytes": self.peak_rss,
            "sampled_peak_threads": self.peak_threads, "samples": self.samples,
            "observed_seconds": time.monotonic() - self.started,
            "process_cpu_seconds": cpu.user + cpu.system - self.cpu_start.user - self.cpu_start.system,
        }


def run(args):
    started = time.monotonic()
    config = config_from_args(args)
    addresses = args.addresses.split(",")
    workers = len(addresses)
    validate_workers(config, workers)
    if len(set(addresses)) != workers or not 0 <= args.rank < workers:
        raise ValueError("Invalid worker addresses or rank")
    if not args.token or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" for c in args.token):
        raise ValueError("Use a nonempty alphanumeric launch token")
    if args.stop_after_epoch is not None and not 1 <= args.stop_after_epoch <= config["epochs"]:
        raise ValueError("stop-after-epoch is outside the configured epoch range")
    root = Path(args.output).resolve()
    invocation = root / "invocations" / args.token
    timeout = config["coordination_timeout_seconds"]

    def expired():
        write_json(invocation / f"timeout-worker-{args.rank}.json", {"status": "timeout"})
        os._exit(124)

    watchdog = None
    if config["timeout_seconds"]:
        watchdog = threading.Timer(config["timeout_seconds"], expired)
        watchdog.daemon = True
        watchdog.start()
    sampler = ResourceSampler()
    try:
        return execute(args, config, addresses, root, invocation, started, sampler, timeout)
    except BaseException:
        write_json(invocation / f"error-worker-{args.rank}.json", {
            "status": "failed", "traceback": traceback.format_exc(),
        })
        raise
    finally:
        if watchdog is not None:
            watchdog.cancel()
        sampler.stop.set()
        sampler.thread.join()


def execute(args, config, addresses, root, invocation, started, sampler, timeout):
    import json

    rank, workers = args.rank, len(addresses)
    # Preserve Slurm/Skiff's granted CUDA device mapping in GPU mode.
    if config["device"] == "cpu":
        os.environ["CUDA_VISIBLE_DEVICES"] = "-1"
    os.environ.setdefault("CUBLAS_WORKSPACE_CONFIG", ":4096:8")
    os.environ["OMP_NUM_THREADS"] = str(config["threads"])
    os.environ["TF_NUM_INTRAOP_THREADS"] = str(config["threads"])
    os.environ["TF_NUM_INTEROP_THREADS"] = "1"
    os.environ.setdefault("TF_GRPC_WORKER_CACHE_THREADS", "2")
    os.environ.setdefault("TF_GRPC_WORKER_CACHE_QUEUES", "1")
    os.environ["TF_CONFIG"] = json.dumps({
        "cluster": {"worker": addresses}, "task": {"type": "worker", "index": rank},
    })
    import numpy as np
    import tensorflow as tf

    phases = {"runtime_import_seconds": time.monotonic() - started}
    before = time.monotonic()
    gpu_devices = tf.config.list_physical_devices("GPU")
    if config["device"] == "cpu":
        tf.config.set_visible_devices([], "GPU")
        gpu_devices = []
    else:
        if len(gpu_devices) != config["gpus_per_node"]:
            raise ValueError(f"Expected exactly {config['gpus_per_node']} granted CUDA GPUs, found {len(gpu_devices)}; no CPU fallback")
        for device in gpu_devices:
            tf.config.experimental.set_memory_growth(device, True)
    tf.config.threading.set_intra_op_parallelism_threads(config["threads"])
    tf.config.threading.set_inter_op_parallelism_threads(1)
    tf.keras.utils.set_random_seed(config["seed"])
    tf.config.experimental.enable_op_determinism()
    devices = {
        "mode": config["device"], "cuda_visible_devices": os.environ.get("CUDA_VISIBLE_DEVICES"),
        "gpus": [{"name": d.name, **tf.config.experimental.get_device_details(d)} for d in gpu_devices],
        "tensorflow_build": tf.sysconfig.get_build_info(),
    }
    phases["device_setup_seconds"] = time.monotonic() - before
    before = time.monotonic()
    arrays, data_id = load_data(config, args.data)
    phases["data_load_verify_seconds"] = time.monotonic() - before
    runtime = {name: importlib.metadata.version(name) for name in (
        "tensorflow", "keras", "numpy", "psutil", "protobuf", "grpcio", "h5py", "ml_dtypes", "optree",
    )}
    contract = {
        "config": config, "workers": workers, "data_identity": data_id,
        "sources": source_identity(), "runtime": runtime, "backend": args.backend,
        "python": platform.python_version(), "machine": platform.machine(),
        "container_identity": os.environ.get("CIFAR_CONTAINER_SHA256", "unspecified"),
        "device_identity": {"mode": config["device"], "gpu_names": [g.get("device_name") for g in devices["gpus"]],
                            "tensorflow_build": devices["tensorflow_build"]},
    }
    contract = json.loads(json.dumps(contract))
    before = time.monotonic()
    if rank == 0:
        if args.resume:
            if read_json(root / "run.json") != contract:
                raise ValueError("Resume refuses a changed workload, environment, source or backend")
            commit = read_json(root / "commit.json")
            checked_artifacts(root, commit["artifacts"])
            if (root / "result.json").exists() and read_json(root / "result.json")["status"] == "complete":
                raise ValueError("Run is already complete; use a new output directory")
        else:
            root.mkdir(parents=True, exist_ok=False)
            write_json(root / "run.json", contract)
            commit = {"epoch": 0, "records": [], "artifacts": {}}
        invocation.mkdir(parents=True, exist_ok=False)
        write_json(invocation / "start.json", {"contract": contract, "commit": commit})
    initial = wait_json(invocation / "start.json", timeout)
    if initial["contract"] != contract:
        raise ValueError("Worker configuration or environment differs from chief")
    commit = initial["commit"]
    checked_artifacts(root, commit["artifacts"])
    start_epoch = commit["epoch"]
    phases["rendezvous_seconds"] = time.monotonic() - before

    before = time.monotonic()
    communication = tf.distribute.experimental.CommunicationOptions(
        implementation=tf.distribute.experimental.CommunicationImplementation.RING,
        timeout_seconds=timeout,
    )
    strategy = tf.distribute.MultiWorkerMirroredStrategy(communication_options=communication)
    replicas = workers * (config["gpus_per_node"] if config["device"] == "gpu" else 1)
    if strategy.num_replicas_in_sync != replicas:
        raise ValueError("TensorFlow replica count differs from requested CPU/GPU topology")
    devices["replica_devices"] = list(strategy.extended.worker_devices)
    if config["device"] == "gpu" and any("GPU:" not in d for d in devices["replica_devices"]):
        raise ValueError("GPU mode created a non-GPU training replica")
    phases["strategy_setup_seconds"] = time.monotonic() - before
    steps = training_steps(config)
    local_batch = config["global_batch_size"] // replicas
    before = time.monotonic()
    with strategy.scope():
        model = build_model(tf, config["width"])
        optimizer = tf.keras.optimizers.SGD(config["learning_rate"], momentum=config["momentum"])
        optimizer.build(model.trainable_variables)
        completed_epoch = tf.Variable(0, dtype=tf.int64, trainable=False)
        checkpoint = tf.train.Checkpoint(model=model, optimizer=optimizer, epoch=completed_epoch)
        if start_epoch:
            record = commit["records"][rank]
            checkpoint.read(str(root / record["checkpoint_prefix"])).assert_consumed()
            if (int(completed_epoch.numpy()) != start_epoch
                    or int(optimizer.iterations.numpy()) != start_epoch * steps
                    or state_digest(model, optimizer) != record["state_sha256"]):
                raise ValueError("Checkpoint did not restore model, optimizer and epoch exactly")
    restored_state = state_digest(model, optimizer)
    phases["model_build_restore_seconds"] = time.monotonic() - before
    startup_seconds = time.monotonic() - started
    epoch_reports = []

    @tf.function
    def training_step(batch):
        def replica_step(images, labels):
            with tf.GradientTape() as tape:
                logits = model(images, training=True)
                losses = tf.keras.losses.sparse_categorical_crossentropy(labels, logits, from_logits=True)
                loss = tf.nn.compute_average_loss(losses, global_batch_size=config["global_batch_size"])
            optimizer.apply_gradients(zip(tape.gradient(loss, model.trainable_variables), model.trainable_variables))
            correct = tf.reduce_sum(tf.cast(tf.equal(tf.argmax(logits, axis=1, output_type=tf.int64), labels), tf.float32))
            return loss, correct

        loss, correct = strategy.run(replica_step, args=batch)
        return (strategy.reduce(tf.distribute.ReduceOp.SUM, loss, axis=None),
                strategy.reduce(tf.distribute.ReduceOp.SUM, correct, axis=None))

    end_epoch = args.stop_after_epoch or config["epochs"]
    if end_epoch < start_epoch or (args.stop_after_epoch is not None and end_epoch == start_epoch):
        raise ValueError("Requested stop epoch is already committed")
    for epoch in range(start_epoch, end_epoch):
        epoch_start = time.monotonic()
        ids = epoch_ids(config, epoch, workers, rank)

        def dataset_fn(context):
            if context.num_input_pipelines != workers or context.input_pipeline_id != rank:
                raise ValueError("Unexpected TensorFlow input pipeline topology")
            dataset = tf.data.Dataset.from_tensor_slices((
                arrays["train_x"][ids], arrays["train_y"][ids].astype(np.int64), ids,
            ))

            def preprocess(image, label, sample_id):
                image = tf.cast(image, tf.float32) / 255.0
                seed = tf.stack([tf.cast(config["seed"] + epoch, tf.int32), tf.cast(sample_id, tf.int32)])
                image = tf.image.stateless_random_flip_left_right(image, seed)
                return (image - 0.5) * 2.0, label

            options = tf.data.Options()
            options.experimental_deterministic = True
            options.experimental_distribute.auto_shard_policy = tf.data.experimental.AutoShardPolicy.OFF
            options.threading.private_threadpool_size = 2
            options.threading.max_intra_op_parallelism = 1
            return dataset.map(preprocess, num_parallel_calls=2).batch(
                local_batch, drop_remainder=True,
            ).with_options(options).prefetch(1)

        distributed = strategy.distribute_datasets_from_function(dataset_fn)
        timings, input_timings, losses, correct_count = [], [], [], 0.0
        iterator = iter(distributed)
        for _ in range(steps):
            before = time.monotonic()
            batch = next(iterator)
            input_timings.append(time.monotonic() - before)
            before = time.monotonic()
            loss, correct = training_step(batch)
            loss_value, correct_value = float(loss.numpy()), float(correct.numpy())
            timings.append(time.monotonic() - before)
            if not np.isfinite(loss_value):
                raise ValueError("Non-finite training loss")
            losses.append(loss_value)
            correct_count += correct_value
        if len(timings) != steps or int(optimizer.iterations.numpy()) != (epoch + 1) * steps:
            raise ValueError("Training step count differs from fixed-work contract")
        training_seconds = time.monotonic() - epoch_start
        checkpoint_start = time.monotonic()
        completed_epoch.assign(epoch + 1)
        epoch_dir = invocation / "checkpoints" / f"epoch-{epoch + 1}" / f"worker-{rank}"
        epoch_dir.mkdir(parents=True, exist_ok=False)
        checkpoint.write(str(epoch_dir / "state"))
        np.save(epoch_dir / "sample-ids.npy", ids, allow_pickle=False)
        report = {
            "rank": rank, "token": args.token, "epoch": epoch + 1, "steps": steps,
            "node": os.environ.get("CIFAR_NODE_NAME", socket.gethostname()),
            "first_epoch_in_invocation": epoch == start_epoch,
            "samples": len(ids), "training_seconds": training_seconds,
            "global_images_per_second": steps * config["global_batch_size"] / training_seconds,
            "loss": float(np.mean(losses)),
            "accuracy": correct_count / (steps * config["global_batch_size"]),
            "step_seconds": timings,
            "input_wait_seconds": input_timings,
            "checkpoint_prefix": str((epoch_dir / "state").relative_to(root)),
            "sample_ids": str((epoch_dir / "sample-ids.npy").relative_to(root)),
            "optimizer_iterations": int(optimizer.iterations.numpy()),
            "state_sha256": state_digest(model, optimizer),
        }
        report["artifacts"] = artifacts_under(root, epoch_dir.glob("*"))
        report["checkpoint_seconds"] = time.monotonic() - checkpoint_start
        write_json(invocation / f"epoch-{epoch + 1}-worker-{rank}.json", report)
        coordination_start = time.monotonic()
        if rank == 0:
            records = [wait_json(invocation / f"epoch-{epoch + 1}-worker-{r}.json", timeout) for r in range(workers)]
            if len({r["state_sha256"] for r in records}) != 1:
                raise ValueError("Workers disagree on model or optimizer state")
            all_ids = np.concatenate([np.load(root / r["sample_ids"], allow_pickle=False) for r in records])
            expected = np.concatenate([epoch_ids(config, epoch, workers, r) for r in range(workers)])
            if not np.array_equal(all_ids, expected):
                raise ValueError("Workers duplicated or omitted their assigned training samples")
            artifacts = {k: v for r in records for k, v in r["artifacts"].items()}
            checked_artifacts(root, artifacts)
            commit = {"epoch": epoch + 1, "records": records, "artifacts": artifacts}
            write_json(root / "epochs" / f"epoch-{epoch + 1}.json", commit)
            write_json(root / "commit.json", commit)
            write_json(invocation / f"committed-{epoch + 1}.json", commit)
        wait_json(invocation / f"committed-{epoch + 1}.json", timeout)
        timing = {"coordination_seconds": time.monotonic() - coordination_start,
                  "epoch_wall_seconds": time.monotonic() - epoch_start}
        write_json(invocation / f"timing-{epoch + 1}-worker-{rank}.json", timing)
        epoch_reports.append({**report, **timing})
        print(f"CIFAR_EPOCH rank={rank} epoch={epoch + 1} loss={report['loss']:.6f} accuracy={report['accuracy']:.4f}", flush=True)

    if gpu_devices:
        devices["allocator_memory"] = {
            f"GPU:{i}": tf.config.experimental.get_memory_info(f"GPU:{i}") for i in range(len(gpu_devices))
        }
        if not all(v["peak"] > 0 for v in devices["allocator_memory"].values()):
            raise ValueError("GPU mode recorded no TensorFlow device allocations")
    worker_report = {
        "rank": rank, "token": args.token, "backend": args.backend,
        "hostname": socket.gethostname(),
        "node": os.environ.get("CIFAR_NODE_NAME", socket.gethostname()),
        "address": addresses[rank], "pid": os.getpid(), "runtime": runtime,
        "cpu_affinity": sorted(os.sched_getaffinity(0)) if hasattr(os, "sched_getaffinity") else None,
        "host_logical_cpu_count": os.cpu_count(),
        "start_epoch": start_epoch, "restored_state_sha256": restored_state,
        "end_epoch": end_epoch, "startup_seconds": startup_seconds,
        "startup_phases": phases, "devices": devices,
        "epochs": epoch_reports, "resources": {"scope": "startup_and_training", **sampler.finish()},
    }
    write_json(invocation / f"worker-{rank}.json", worker_report)
    if rank == 0:
        reports = [wait_json(invocation / f"worker-{r}.json", timeout) for r in range(workers)]
        if args.backend != "local" and workers > 1 and len({r["node"] for r in reports}) != workers:
            raise ValueError("Distributed platform run requires distinct physical nodes")
        status = "paused" if end_epoch < config["epochs"] else "complete"
        result = {"status": status, "token": args.token, "workers": reports, "contract": contract}
        if status == "complete":
            result["evaluation"] = evaluate(tf, np, model, arrays, config, root)
        result["artifacts"] = artifacts_under(root, [p for p in root.rglob("*") if p.is_file()
            and p.name != "result.json" and (p.suffix in (".npy", ".index", ".keras", ".json") or ".data-" in p.name)])
        result["wall_seconds"] = time.monotonic() - started
        write_json(invocation / "result.json", result)
        write_json(root / "result.json", result)
        print(f"CIFAR_RESULT status={status} output={root}", flush=True)
    wait_json(invocation / "result.json", timeout)
    return 0


def evaluate(tf, np, model, arrays, config, root):
    started = time.monotonic()
    inference = build_model(tf, config["width"])
    inference.set_weights(model.get_weights())
    output = {}
    for split in ("validation", "test"):
        split_started = time.monotonic()
        losses, correct, count = 0.0, 0, 0
        x, y = arrays[f"{split}_x"], arrays[f"{split}_y"]
        for offset in range(0, len(y), config["global_batch_size"]):
            images = (x[offset:offset + config["global_batch_size"]].astype(np.float32) / 255.0 - 0.5) * 2.0
            labels = y[offset:offset + config["global_batch_size"]]
            logits = inference(images, training=False).numpy()
            if not np.all(np.isfinite(logits)):
                raise ValueError("Non-finite evaluation predictions")
            losses += float(tf.reduce_sum(tf.keras.losses.sparse_categorical_crossentropy(labels, logits, from_logits=True)).numpy())
            correct += int(np.sum(logits.argmax(axis=1) == labels))
            count += len(labels)
        output[split] = {"samples": count, "loss": losses / count, "accuracy": correct / count,
                         "seconds": time.monotonic() - split_started}
    before = time.monotonic()
    inference.save(root / "model.keras")
    output["export_seconds"] = time.monotonic() - before
    before = time.monotonic()
    reloaded = tf.keras.models.load_model(root / "model.keras", compile=False)
    probe = (arrays["test_x"][:16].astype(np.float32) / 255.0 - 0.5) * 2.0
    expected = inference(probe, training=False).numpy()
    actual = reloaded(probe, training=False).numpy()
    np.testing.assert_allclose(actual, expected, rtol=1e-6, atol=1e-6)
    output["reload_verify_seconds"] = time.monotonic() - before
    output["reload_max_absolute_error"] = float(np.max(np.abs(actual - expected)))
    output["seconds"] = time.monotonic() - started
    return output
