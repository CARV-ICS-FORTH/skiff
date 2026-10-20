package eu.knotplatform.sparkknot;

import org.apache.spark.sql.SparkSession;
import org.apache.spark.sql.TPCDSTables;

/** Java bridge used by the PySpark driver to call Spark's TPC-DS generator. */
public final class SparkKnotTPCDS {
    private SparkKnotTPCDS() {}

    public static void generate(
            SparkSession spark,
            String dsdgenDirectory,
            String location,
            int scaleFactor,
            int partitions) {
        TPCDSTables tables = new TPCDSTables(spark, dsdgenDirectory, scaleFactor);
        tables.genData(
                location,
                "parquet",
                true,
                false,
                false,
                false,
                "",
                partitions);
    }
}
