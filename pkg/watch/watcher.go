package watch

import (
	"fmt"

	"os"

	"github.com/harvester/rancherd/pkg/controller"
	"github.com/rancher/lasso/pkg/cache"
	"github.com/rancher/lasso/pkg/client"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	KubeletKubeConfig = "/var/lib/rancher/rke2/agent/kubelet.kubeconfig"
)

func Setup() error {
	nodeName, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("failed to get hostname: %w", err)
	}
	val, ok := os.LookupEnv("RANCHERD_DEBUG")
	if ok && val == "true" {
		logrus.SetLevel(logrus.DebugLevel)
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = KubeletKubeConfig
	configOverrides := &clientcmd.ConfigOverrides{}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		logrus.Fatal(err)
	}

	clientFactory, err := client.NewSharedClientFactory(config, nil)
	if err != nil {
		return err
	}

	cacheFactory := cache.NewSharedCachedFactory(clientFactory, &cache.SharedCacheFactoryOptions{
		KindTweakList: map[schema.GroupVersionKind]cache.TweakListOptionsFunc{
			corev1.SchemeGroupVersion.WithKind("Node"): generateTweakList(nodeName),
		},
	})
	k8sclient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("error creating kubernetes client: %w", err)
	}
	ctx := signals.SetupSignalContext()

	coreFactory, err := core.NewFactoryFromConfigWithOptions(config, &core.FactoryOptions{
		SharedCacheFactory: cacheFactory,
	})

	if err != nil {
		return fmt.Errorf("error generating core factory: %w", err)
	}

	if err := controller.Register(ctx, coreFactory.Core().V1().Node(), k8sclient.CoreV1().Nodes(), nodeName); err != nil {
		return fmt.Errorf("error registering controller: %w", err)
	}

	if err := coreFactory.Start(ctx, 1); err != nil {
		return fmt.Errorf("error starting core factory: %w", err)
	}
	<-ctx.Done()
	return nil
}

func generateTweakList(nodeName string) cache.TweakListOptionsFunc {
	return func(options *metav1.ListOptions) {
		// Customize the list options here if needed
		options.FieldSelector = fmt.Sprintf("metadata.name=%s", nodeName)
	}
}
