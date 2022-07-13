package prometheus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/user"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
)

type podMetadata struct {
	ResourceVersion string `json:"resourceVersion"`
	SelfLink        string `json:"selfLink"`
}

type podResponse struct {
	Kind       string        `json:"kind"`
	APIVersion string        `json:"apiVersion"`
	Metadata   podMetadata   `json:"metadata"`
	Items      []*corev1.Pod `json:"items,string,omitempty"`
}

const cAdvisorPodListDefaultInterval = 60

// loadConfig parses a kubeconfig from a file and returns a Kubernetes
// client. It does not support extensions or client auth providers.
func loadConfig(kubeconfigPath string) (*rest.Config, error) {
	if kubeconfigPath == "" {
		return rest.InClusterConfig()
	}

	return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
}

func (p *Prometheus) startK8s(ctx context.Context) error {
	config, err := loadConfig(p.KubeConfig)
	if err != nil {
		return fmt.Errorf("failed to get rest.Config from %v - %v", p.KubeConfig, err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		u, err := user.Current()
		if err != nil {
			return fmt.Errorf("failed to get current user - %v", err)
		}

		kubeconfig := filepath.Join(u.HomeDir, ".kube/config")

		config, err = loadConfig(kubeconfig)
		if err != nil {
			return fmt.Errorf("failed to get rest.Config from %v - %v", kubeconfig, err)
		}

		client, err = kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("failed to get kubernetes client - %v", err)
		}
	}

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				if p.isNodeScrapeScope {
					err = p.cAdvisor(ctx, config.BearerToken)
					if err != nil {
						p.Log.Errorf("Unable to monitor pods with node scrape scope: %s", err.Error())
					}
				} else {
					p.Log.Debugf("start to watch pod in cluster mode")
					err = p.watchPodFromInformer(ctx, client)
					if err != nil {
						p.Log.Errorf("Unable to watch resources: %s", err.Error())
					}
				}
			}
		}
	}()

	return nil
}

// An edge case exists if a pod goes offline at the same time a new pod is created
// (without the scrape annotations). K8s may re-assign the old pod ip to the non-scrape
// pod, causing errors in the logs. This is only true if the pod going offline is not
// directed to do so by K8s.
func (p *Prometheus) watchPodFromInformer(ctx context.Context, client *kubernetes.Clientset) error {
	optionsModifier := func(options *metav1.ListOptions) {
		options.FieldSelector = p.KubernetesFieldSelector
		options.LabelSelector = p.KubernetesLabelSelector
	}

	queue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "queue")

	podInformer := cache.NewSharedInformer(
		cache.NewFilteredListWatchFromClient(client.CoreV1().RESTClient(), "pods", p.PodNamespace, optionsModifier),
		&corev1.Pod{}, time.Minute*15,
	)

	podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod, ok := newObj.(*corev1.Pod)
			if !ok {
				p.Log.Warnf("expect type pod")
				return
			}

			if pod.Annotations["prometheus.io/scrape"] != "true" {
				p.Log.Debug("%s/%s not found prometheus scrape annotations, skip UpdateFunc", pod.Namespace, pod.Name)
				return
			}

			if !podReady(pod.Status.ContainerStatuses) {
				p.Log.Debugf("%s/%s not ready,skip UpdateFunc", pod.Namespace, pod.Name)
				return
			}

			queue.Add(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
		},
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				p.Log.Warnf("expect type pod")
				return
			}

			if pod.Annotations["prometheus.io/scrape"] != "true" {
				p.Log.Debug("%s/%s not found prometheus scrape annotations, skip AddFunc", pod.Namespace, pod.Name)
				return
			}

			if !podReady(pod.Status.ContainerStatuses) {
				p.Log.Debugf("%s/%s not ready,skip AddFunc", pod.Namespace, pod.Name)
				return
			}

			queue.Add(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				p.Log.Warnf("expect type pod")
				return
			}

			if pod.Annotations["prometheus.io/scrape"] != "true" {
				p.Log.Debug("%s/%s not found prometheus scrape annotations, skip DeleteFunc", pod.Namespace, pod.Name)
				return
			}

			unregisterPod(pod, p)
		},
	})

	go podInformer.Run(ctx.Done())

	if !cache.WaitForCacheSync(ctx.Done(), podInformer.HasSynced) {
		return fmt.Errorf("failt to sync informer cache")
	}

	go func() {
		for {
			item, shutdown := queue.Get()
			if shutdown {
				p.Log.Infof("informer shutdown")
				return
			}

			queue.Done(item)

			key := item.(string)

			obj, exist, err := podInformer.GetStore().GetByKey(key)
			if err != nil {
				p.Log.Errorf("get %s from cache err: %v", key, err)
				continue
			}

			if !exist {
				p.Log.Warnf("%s object not exist", key)
				continue
			}

			pod := obj.(*corev1.Pod)

			registerPod(pod, p)
		}
	}()

	<-ctx.Done()
	p.Log.Infof("context close, shutdown queue")
	queue.ShutDown()
	return nil
}

// An edge case exists if a pod goes offline at the same time a new pod is created
// (without the scrape annotations). K8s may re-assign the old pod ip to the non-scrape
// pod, causing errors in the logs. This is only true if the pod going offline is not
// directed to do so by K8s.
func (p *Prometheus) watchPod(ctx context.Context, client *kubernetes.Clientset) error {
	watcher, err := client.CoreV1().Pods(p.PodNamespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: p.KubernetesLabelSelector,
		FieldSelector: p.KubernetesFieldSelector,
	})
	if err != nil {
		p.Log.Debugf("client watch pod err: %v", err)
		return err
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			for event := range watcher.ResultChan() {
				pod, ok := event.Object.(*corev1.Pod)
				if !ok {
					return fmt.Errorf("unexpected object when getting pods")
				}

				// If the pod is not "ready", there will be no ip associated with it.
				if pod.Annotations["prometheus.io/scrape"] != "true" {
					p.Log.Debug("%s/%s not found prometheus scrape annotations, skip...", pod.Namespace, pod.Name)
					continue
				}

				switch event.Type {
				case watch.Added:
					registerPod(pod, p)
				case watch.Deleted:
					unregisterPod(pod, p)
				case watch.Modified:
					// To avoid multiple actions for each event, unregister on the first event
					// in the delete sequence, when the containers are still "ready".
					if pod.GetDeletionTimestamp() != nil {
						unregisterPod(pod, p)
					} else {
						registerPod(pod, p)
					}
				}
			}

			return nil
		}

	}
}

func (p *Prometheus) cAdvisor(ctx context.Context, bearerToken string) error {
	// The request will be the same each time
	podsURL := fmt.Sprintf("https://%s:10250/pods", p.NodeIP)
	req, err := http.NewRequest("GET", podsURL, nil)
	if err != nil {
		return fmt.Errorf("error when creating request to %s to get pod list: %w", podsURL, err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Add("Accept", "application/json")

	// Update right away so code is not waiting the length of the specified scrape interval initially
	err = updateCadvisorPodList(p, req)
	if err != nil {
		return fmt.Errorf("error initially updating pod list: %w", err)
	}

	scrapeInterval := cAdvisorPodListDefaultInterval
	if p.PodScrapeInterval != 0 {
		scrapeInterval = p.PodScrapeInterval
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(scrapeInterval) * time.Second):
			err := updateCadvisorPodList(p, req)
			if err != nil {
				return fmt.Errorf("error updating pod list: %w", err)
			}
		}
	}
}

func updateCadvisorPodList(p *Prometheus, req *http.Request) error {
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	httpClient := http.Client{}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("error when making request for pod list: %w", err)
	}

	// If err is nil, still check response code
	if resp.StatusCode != 200 {
		return fmt.Errorf("error when making request for pod list with status %s", resp.Status)
	}

	defer resp.Body.Close()

	cadvisorPodsResponse := podResponse{}

	// Will have expected type errors for some parts of corev1.Pod struct for some unused fields
	// Instead have nil checks for every used field in case of incorrect decoding
	if err := json.NewDecoder(resp.Body).Decode(&cadvisorPodsResponse); err != nil {
		return fmt.Errorf("decoding response failed: %v", err)
	}
	pods := cadvisorPodsResponse.Items

	// Register pod only if it has an annotation to scrape, if it is ready,
	// and if namespace and selectors are specified and match
	for _, pod := range pods {
		if necessaryPodFieldsArePresent(pod) &&
			pod.Annotations["prometheus.io/scrape"] == "true" &&
			podReady(pod.Status.ContainerStatuses) &&
			podHasMatchingNamespace(pod, p) &&
			podHasMatchingLabelSelector(pod, p.podLabelSelector) &&
			podHasMatchingFieldSelector(pod, p.podFieldSelector) {
			registerPod(pod, p)
		}
	}

	// No errors
	return nil
}

func necessaryPodFieldsArePresent(pod *corev1.Pod) bool {
	return pod.Annotations != nil &&
		pod.Labels != nil &&
		pod.Status.ContainerStatuses != nil
}

/* See the docs on kubernetes label selectors:
 * https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors
 */
func podHasMatchingLabelSelector(pod *corev1.Pod, labelSelector labels.Selector) bool {
	if labelSelector == nil {
		return true
	}

	var labelsSet labels.Set = pod.Labels
	return labelSelector.Matches(labelsSet)
}

/* See ToSelectableFields() for list of fields that are selectable:
 * https://github.com/kubernetes/kubernetes/release-1.20/pkg/registry/core/pod/strategy.go
 * See docs on kubernetes field selectors:
 * https://kubernetes.io/docs/concepts/overview/working-with-objects/field-selectors/
 */
func podHasMatchingFieldSelector(pod *corev1.Pod, fieldSelector fields.Selector) bool {
	if fieldSelector == nil {
		return true
	}

	fieldsSet := make(fields.Set)
	fieldsSet["spec.nodeName"] = pod.Spec.NodeName
	fieldsSet["spec.restartPolicy"] = string(pod.Spec.RestartPolicy)
	fieldsSet["spec.schedulerName"] = pod.Spec.SchedulerName
	fieldsSet["spec.serviceAccountName"] = pod.Spec.ServiceAccountName
	fieldsSet["status.phase"] = string(pod.Status.Phase)
	fieldsSet["status.podIP"] = pod.Status.PodIP
	fieldsSet["status.nominatedNodeName"] = pod.Status.NominatedNodeName

	return fieldSelector.Matches(fieldsSet)
}

/*
 * If a namespace is specified and the pod doesn't have that namespace, return false
 * Else return true
 */
func podHasMatchingNamespace(pod *corev1.Pod, p *Prometheus) bool {
	return !(p.PodNamespace != "" && pod.Namespace != p.PodNamespace)
}

func podReady(statuss []corev1.ContainerStatus) bool {
	if len(statuss) == 0 {
		return false
	}
	for _, cs := range statuss {
		if !cs.Ready {
			return false
		}
	}
	return true
}

func registerPod(pod *corev1.Pod, p *Prometheus) {
	p.lock.Lock()
	defer p.lock.Unlock()

	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	p.Log.Debugf("start to register pod %s", key)

	if p.kubernetesPods == nil {
		p.kubernetesPods = map[string]URLAndAddress{}
	}

	targetURL, err := getScrapeURL(pod)
	if err != nil {
		p.Log.Errorf("could not parse URL: %s", err)
		return
	} else if targetURL == nil {
		p.Log.Debugf("get targetURL == nil skip ...")
		return
	}

	p.Log.Debugf("will scrape metrics from %q", targetURL.String())
	// add annotation as metrics tags
	tags := pod.Annotations
	if tags == nil {
		tags = map[string]string{}
	}

	tags["pod_name"] = pod.Name
	tags["pod_namespace"] = pod.Namespace
	// add labels as metrics tags
	for k, v := range pod.Labels {
		tags[k] = v
	}
	podURL := p.AddressToURL(targetURL, targetURL.Hostname())

	p.kubernetesPods[key] = URLAndAddress{
		URL:         podURL,
		Address:     targetURL.Hostname(),
		OriginalURL: targetURL,
		Tags:        tags,
	}
}

func getScrapeURL(pod *corev1.Pod) (*url.URL, error) {
	ip := pod.Status.PodIP
	if ip == "" {
		// return as if scrape was disabled, we will be notified again once the pod
		// has an IP
		return nil, nil
	}

	scheme := pod.Annotations["prometheus.io/scheme"]
	pathAndQuery := pod.Annotations["prometheus.io/path"]
	port := pod.Annotations["prometheus.io/port"]

	if scheme == "" {
		scheme = "http"
	}
	if port == "" {
		port = "9102"
	}
	if pathAndQuery == "" {
		pathAndQuery = "/metrics"
	}

	base, err := url.Parse(pathAndQuery)
	if err != nil {
		return nil, err
	}

	base.Scheme = scheme
	base.Host = net.JoinHostPort(ip, port)

	return base, nil
}

func unregisterPod(pod *corev1.Pod, p *Prometheus) {
	key := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)

	p.Log.Debugf("registered a delete request for %q in namespace %q", pod.Name, pod.Namespace)

	p.lock.Lock()
	defer p.lock.Unlock()
	if targetURL, ok := p.kubernetesPods[key]; ok {
		p.Log.Debugf("will stop scraping for %v", targetURL.URL)
		delete(p.kubernetesPods, key)
	}
}
