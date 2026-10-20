package eu.knotplatform.sparkknot;

/** Discover CUDA-visible logical ordinals without a compile-time cuDF dependency. */
public final class GpuDiscovery {
    public static void main(String[] args) throws Exception {
        Class<?> cuda = Class.forName("ai.rapids.cudf.Cuda");
        int count = ((Number) cuda.getMethod("getDeviceCount").invoke(null)).intValue();
        if (count < 1) {
            throw new IllegalStateException("No CUDA-visible GPU; check allocation, driver and container GPU access");
        }
        StringBuilder json = new StringBuilder("{\"name\":\"gpu\",\"addresses\":[");
        for (int i = 0; i < count; i++) {
            if (i > 0) json.append(',');
            json.append('"').append(i).append('"');
        }
        System.out.println(json.append("]}"));
    }
}
