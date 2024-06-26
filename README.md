# Egress IP validator

1. Deploy `k8s.yaml` to create a namespace, service, RBAC and a `ServiceMonitor`. This is required to consume the metrics generated by the following workload.
2. Deploy `pod.yaml` which will create a work load which connects to a target that reports the source IP seen. The pod will poll the target contineously until an EgressIP is seen. It also records the number of times a non-Egress IP is seen.
See `pod.yaml` for customizations via env vars. You must define the target IP and port. You must also define the expected Egress IPs.
3. Within OCP console metrics, you may find the following metrics:
- "scale_eip_startup_latency_total"
Time it takes in seconds for a connection to have a source IP of EgressIP at startup with polling interval of X seconds, where X is defined as an env var in `pod.yaml`
- "scale_eip_total"
Increments every time EgressIP seen as source IP - increments every X seconds if seen, where X is defined as an env var in `pod.yaml`
- "scale_non_eip_total"
Increments every time EgressIP not seen as source IP - increments every X seconds if seen, where X is defined as an env var in `pod.yaml`
- "scale_failure_total"
  Increments every time there a connection failure (not status 200) - increments every X seconds if seen, where X is defined as an env var in `pod.yaml`
