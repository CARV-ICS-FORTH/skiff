"""Small real-Spark check using a local managed PySpark gateway. No cluster/S3."""

import sys
import tempfile
from pathlib import Path

from pyspark.sql import SparkSession

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
import benchmark


def main():
    spark = SparkSession.builder.appName("analytics-spark-local-validation").getOrCreate()
    spark.sparkContext.setLogLevel("WARN")
    try:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            benchmark.TPCDS_TABLES = ("store_sales",)
            benchmark.QUERY_DIRECTORY = root / "queries"
            benchmark.QUERY_DIRECTORY.mkdir()
            (benchmark.QUERY_DIRECTORY / "q1.sql").write_text(
                "SELECT id % 3 AS category, SUM(id) AS total FROM store_sales GROUP BY id % 3")
            (benchmark.QUERY_DIRECTORY / "q2.sql").write_text(
                "SELECT COUNT(*) AS matches FROM store_sales a JOIN store_sales b ON a.id = b.id")
            spark.range(30).repartition(2).write.parquet(str(root / "data/store_sales"))
            timings, stats = benchmark.run_queries(
                spark, str(root / "data"), str(root / "results"), "q1,q2", 3, "cpu")
            assert len(timings) == 6
            assert [(row["iteration"], row["query"], row["result_rows"]) for row in timings] == [
                (iteration, name, rows) for iteration in range(1, 4) for name, rows in (("q1", 3), ("q2", 1))]
            assert set(stats) == {"q1", "q2"}
            assert benchmark.acceleration_summary("cpu", timings)["gpu_query_runs"] == 0
            assert spark.read.json(str(root / "results/query-timings")).count() == 6
            assert spark.read.json(str(root / "results/query-plans")).count() == 2
            try:
                benchmark.run_queries(spark, str(root / "data"), str(root / "gpu"), "q1", 1, "gpu")
            except ValueError as error:
                assert "CPU fallback" in str(error)
            else:
                raise AssertionError("CPU-only Spark was accepted as GPU execution")
    finally:
        gateway = spark.sparkContext._gateway
        spark.stop()
        gateway.shutdown()
    print("SPARK_LOCAL_VALIDATION_PASS: SQL iterations, output artifacts, plans and GPU fallback rejection")


if __name__ == "__main__":
    main()
