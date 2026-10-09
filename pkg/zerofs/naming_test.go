package zerofs

import (
	"regexp"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var rfc1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

func expectValidName(name string) {
	gomega.Expect(len(name)).To(gomega.BeNumerically("<=", maxK8sNameLen), name)
	gomega.Expect(rfc1123Subdomain.MatchString(name)).To(gomega.BeTrue(), name)
}

var _ = ginkgo.Describe("k8sName", func() {
	maxLen := 55

	ginkgo.It("should leave volume ids that are already valid untouched", func() {
		gomega.Expect(k8sName("pvc-12345", maxLen)).To(gomega.Equal("pvc-12345"))
		gomega.Expect(k8sName("sanity-node-full-1.abc", maxLen)).To(gomega.Equal("sanity-node-full-1.abc"))
	})

	ginkgo.It("should lowercase volume ids", func() {
		name := k8sName("sanity-controller-create-8EFF2AC8-B92C3188", maxLen)
		expectValidName(name)
		gomega.Expect(name).To(gomega.HavePrefix("sanity-controller-create-8eff2ac8-b92c3188-"))
	})

	ginkgo.It("should replace characters that are not allowed in a name", func() {
		name := k8sName("weird_volume/id:1", maxLen)
		expectValidName(name)
		gomega.Expect(name).To(gomega.HavePrefix("weird-volume-id-1-"))
	})

	ginkgo.It("should trim leading and trailing separators", func() {
		expectValidName(k8sName("-._volume", maxLen))
		expectValidName(k8sName("!!!", maxLen))
	})

	ginkgo.It("should shorten volume ids that are too long", func() {
		name := k8sName(strings.Repeat("a", 200), maxLen)
		expectValidName(name)
		gomega.Expect(len(name)).To(gomega.Equal(maxLen))
		gomega.Expect(name).To(gomega.HavePrefix(strings.Repeat("a", maxLen-9)))
	})

	ginkgo.It("should keep rewritten volume ids distinct", func() {
		longA := strings.Repeat("a", 200) + "-one"
		longB := strings.Repeat("a", 200) + "-two"
		gomega.Expect(k8sName(longA, maxLen)).ToNot(gomega.Equal(k8sName(longB, maxLen)))

		gomega.Expect(k8sName("ABC", maxLen)).ToNot(gomega.Equal(k8sName("abc", maxLen)))
	})

	ginkgo.It("should produce names that are valid for every managed object", func() {
		manager := NewManager("default", "/var/lib/zerofs-csi", "ghcr.io/barre/zerofs:1.0.4")

		ids := []string{
			"pvc-12345",
			"sanity-controller-create-maxlen-8EFF2AC8-" + strings.Repeat("a", 79),
			"sanity-controller-create-single-no-capacity-8EFF2AC8-B92C3188",
			"sanity-node-full-1-8EFF2AC8-B92C3188",
			strings.Repeat("volume-", 30),
		}

		for _, id := range ids {
			expectValidName(manager.GetServiceName(id))
			expectValidName(manager.GetDeploymentName(id))
			expectValidName(manager.GetSecretName(id))
			gomega.Expect(manager.GetDeploymentName(id)).To(gomega.Equal(manager.GetServiceName(id)), id)
		}
	})
})
