"""Real standalone executor probe check; run with a separately started master/worker."""
import os
import sys
from pathlib import Path

from pyspark import cloudpickle
from pyspark.sql import SparkSession

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / 'slurm'))
import driver

# The production adapter runs as __main__, so its callback is sent by value.
cloudpickle.register_pickle_by_value(driver)
spark = SparkSession.builder.appName('standalone-resource-probe-regression').getOrCreate()
spark.sparkContext.setLogLevel('WARN')
try:
    rows = spark.sparkContext.parallelize(range(2), 2).barrier().mapPartitions(driver.partition_probe).collect()
    probe, hosts = driver.verify_probe(rows, [os.uname().nodename], 2, os.environ['SLURM_JOB_ID'])
    assert len(hosts) == 1
    assert all(row['execution_environment']['cpu_affinity'] for row in probe)
    assert all('benchmark.py' in {Path(k).name for k in row['execution_environment']['source_sha256']}
               for row in probe)
    print('SPARK_STANDALONE_PROBE_PASS: resource callback imported in independent worker directory')
finally:
    spark.stop()
