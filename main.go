package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"

	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	ref "k8s.io/client-go/tools/reference"
)

type WatchEvent struct {
	Type watch.EventType
	Pod  *v1.Pod
}

var (
	minWatchTimeout = 5 * time.Minute
	eventReason     = "ContainerRestart"
	clientset       *kubernetes.Clientset
)

func main() {
	masterURL := flag.String("master", "", "kubernetes api server url")
	kubeconfigPath := flag.String("kubeconfig", "", "path to kubeconfig file")
	flag.StringVar(&eventReason, "eventReason", "ContainerRestart", "event reason")
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfigPath)
	if err != nil {
		log.Fatalln(err)
	}

	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalln(err)
	}

	pods := make(map[types.UID]*v1.Pod, 1000)
	watchEventCh := make(chan WatchEvent, 128)
	go podWatcher(watchEventCh)

	for watchEvent := range watchEventCh {
		pod := watchEvent.Pod
		if watchEvent.Type == watch.Deleted {
			delete(pods, pod.UID)
		} else {
			prevPod, prevExist := pods[pod.UID]
			pods[pod.UID] = pod

			if prevExist {
				handlePodUpdate(pod, prevPod)
			}
		}
	}
}

func podWatcher(c chan WatchEvent) {
	for {
		err := internalPodWatcher(c)
		if statusErr, ok := err.(*apierrs.StatusError); ok {
			if statusErr.ErrStatus.Reason == metav1.StatusReasonExpired {
				log.Println("podWatcher:", err, "Restarting watch")
				continue
			}
		}

		log.Fatalln(err)
	}
}

func internalPodWatcher(c chan WatchEvent) error {
	list, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	for _, pod := range list.Items {
		c <- WatchEvent{
			Type: watch.Added,
			Pod:  &pod,
		}
	}

	resourceVersion := list.ResourceVersion

	for {
		log.Println("podWatcher: watching since", resourceVersion)

		timeoutSeconds := int64(minWatchTimeout.Seconds() * (rand.Float64() + 1.0))
		watcher, err := clientset.CoreV1().Pods("").Watch(context.TODO(), metav1.ListOptions{
			ResourceVersion: resourceVersion,
			TimeoutSeconds:  &timeoutSeconds,
		})
		if err != nil {
			return err
		}

		for watchEvent := range watcher.ResultChan() {
			if watchEvent.Type == watch.Error {
				return apierrs.FromObject(watchEvent.Object)
			}

			pod, ok := watchEvent.Object.(*v1.Pod)
			if !ok {
				log.Println("podWatcher: unexpected kind:", watchEvent.Object.GetObjectKind().GroupVersionKind())
				continue
			}

			resourceVersion = pod.ResourceVersion
			c <- WatchEvent{
				Type: watchEvent.Type,
				Pod:  pod,
			}
		}
	}
}

func handlePodUpdate(pod *v1.Pod, prevPod *v1.Pod) {
	handleContainersUpdate(pod, pod.Status.ContainerStatuses, prevPod.Status.ContainerStatuses)
	handleContainersUpdate(pod, pod.Status.InitContainerStatuses, prevPod.Status.InitContainerStatuses)
}

func handleContainersUpdate(pod *v1.Pod, containerStatuses []v1.ContainerStatus, prevContainerStatuses []v1.ContainerStatus) {
	prevContainerStatusesMap := make(map[string]*v1.ContainerStatus, len(prevContainerStatuses))
	for _, containerStatus := range prevContainerStatuses {
		prevContainerStatusesMap[containerStatus.Name] = &containerStatus
	}

	for _, containerStatus := range containerStatuses {
		prevContainerStatus, ok := prevContainerStatusesMap[containerStatus.Name]
		if !ok {
			continue
		}
		if containerStatus.RestartCount > prevContainerStatus.RestartCount {
			handleContainerRestart(pod, &containerStatus)
		}
	}
}

func handleContainerRestart(pod *v1.Pod, containerStatus *v1.ContainerStatus) {
	t := containerStatus.LastTerminationState.Terminated.FinishedAt
	ref, err := ref.GetReference(scheme.Scheme, pod)
	if err != nil {
		log.Printf("Could not construct reference to: '%#v' due to: '%v'", pod, err)
	}

	msg := formatMessage(pod, containerStatus)
	log.Println(msg)

	event := &v1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%v.%x", pod.Name, time.Now().UnixNano()),
			Namespace: pod.Namespace,
		},
		InvolvedObject: *ref,
		Reason:         eventReason,
		Message:        msg,
		FirstTimestamp: t,
		LastTimestamp:  t,
		Count:          1,
		Type:           "Warning",
		Source: v1.EventSource{
			Component: "kube-restart-monitor",
		},
	}

	_, err = clientset.CoreV1().Events(pod.Namespace).Create(context.TODO(), event, metav1.CreateOptions{})
	if err != nil {
		log.Printf("Unable to write event: '%v'", err)
	}
}

func formatMessage(pod *v1.Pod, containerStatus *v1.ContainerStatus) string {
	t := containerStatus.LastTerminationState.Terminated
	msg := fmt.Sprintf("Container %s in pod %s/%s restarted.\nReason: %s, exit code: %d.",
		containerStatus.Name, pod.Namespace, pod.Name, t.Reason, t.ExitCode)
	if t.Message != "" {
		msg += "\nMessage: " + t.Message
	}
	return msg
}
