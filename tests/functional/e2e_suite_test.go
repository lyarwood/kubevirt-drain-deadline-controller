package functional_test

import (
	"context"
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"kubevirt.io/client-go/kubecli"
)

var (
	virtClient    kubecli.KubevirtClient
	testNamespace string
	ctx           = context.Background()
)

func TestFunctional(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Deadline Eviction Functional Suite")
}

var _ = BeforeSuite(func() {
	kubeconfigPath := os.Getenv("KUBECONFIG")
	if _, err := os.Stat(kubeconfigPath); os.IsNotExist(err) {
		Skip("KUBECONFIG not set or not found")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	Expect(err).NotTo(HaveOccurred())

	virtClient, err = kubecli.GetKubevirtClientFromRESTConfig(config)
	Expect(err).NotTo(HaveOccurred())

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "deadline-eviction-test-",
		},
	}
	ns, err = virtClient.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	testNamespace = ns.Name
})

var _ = AfterSuite(func() {
	if testNamespace != "" {
		_ = virtClient.CoreV1().Namespaces().Delete(ctx, testNamespace, metav1.DeleteOptions{})
	}
})
