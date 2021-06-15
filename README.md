# kube-restart-monitor

Watches Kubernetes API server for changed Pods and if restartCount in container status increases, creates Event.

## Usage

```
  -eventReason string
    	event reason (default "ContainerRestart")
  -kubeconfig string
    	path to kubeconfig file
  -master string
    	kubernetes api server url
```
