package component_test

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/onsi/gomega/gbytes"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"github.com/pivotal-cf/kiln/internal/baking"
	"github.com/pivotal-cf/kiln/internal/builder"
	"github.com/pivotal-cf/kiln/internal/component"
)

var _ = Describe("LocalReleaseDirectory", func() {
	var (
		localReleaseDirectory component.LocalReleaseDirectory
		noConfirm             bool
		releasesDir           string
		releaseFile           string
		fakeLogger            *log.Logger
		logBuf                *gbytes.Buffer
	)

	BeforeEach(func() {
		var err error
		releasesDir, err = ioutil.TempDir("", "releases")
		noConfirm = true
		Expect(err).NotTo(HaveOccurred())

		releaseFile = filepath.Join(releasesDir, "some-release.tgz")

		logBuf = gbytes.NewBuffer()
		fakeLogger = log.New(logBuf, "", 0)

		releaseManifestReader := builder.NewReleaseManifestReader(osfs.New(""))
		releasesService := baking.NewReleasesService(fakeLogger, releaseManifestReader)

		localReleaseDirectory = component.NewLocalReleaseDirectory(fakeLogger, releasesService)
	})

	AfterEach(func() {
		_ = os.RemoveAll(releasesDir)
	})

	Describe("GetLocalReleases", func() {
		Context("when releases exist in the releases dir", func() {
			BeforeEach(func() {
				fixtureContent, err := ioutil.ReadFile(filepath.Join("fixtures", "some-release.tgz"))
				Expect(err).NotTo(HaveOccurred())
				err = ioutil.WriteFile(releaseFile, fixtureContent, 0755)
				Expect(err).NotTo(HaveOccurred())
			})

			It("returns a map of releases to locations", func() {
				releases, err := localReleaseDirectory.GetLocalReleases(releasesDir)
				Expect(err).NotTo(HaveOccurred())
				Expect(releases).To(HaveLen(1))
				Expect(releases).To(ConsistOf(
					component.Local{
						Lock: component.Lock{
							Name:    "some-release",
							Version: "1.2.3",
							SHA1:    "6d96f7c98610fa6d8e7f45271111221b5b8497a2",
						},
						LocalPath: releaseFile,
					},
				))
			})
		})

		Context("when there are no local releases", func() {
			It("returns an empty slice", func() {
				releases, err := localReleaseDirectory.GetLocalReleases(releasesDir)
				Expect(err).NotTo(HaveOccurred())
				Expect(releases).To(HaveLen(0))
			})
		})

		Context("when the releases directory does not exist", func() {
			It("returns an empty slice", func() {
				_, err := localReleaseDirectory.GetLocalReleases("some-invalid-directory")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("some-invalid-directory"))
			})
		})
	})

	Describe("DeleteExtraReleases", func() {
		var extraFilePath, zFilePath string
		BeforeEach(func() {
			extraFilePath = filepath.Join(releasesDir, "extra-release-0.0-os-0-0.0.0.tgz")
			err := ioutil.WriteFile(extraFilePath, []byte("abc"), 0644)
			Expect(err).NotTo(HaveOccurred())

			zFilePath = filepath.Join(releasesDir, "z-release-0.0-os-0-0.0.0.tgz")
			err = ioutil.WriteFile(zFilePath, []byte("xyz"), 0644)
			Expect(err).NotTo(HaveOccurred())
		})

		It("deletes specified files", func() {
			extraReleaseID := component.Spec{Name: "extra-release", Version: "0.0"}
			extraRelease := component.Local{Lock: extraReleaseID.Lock(), LocalPath: extraFilePath}

			err := localReleaseDirectory.DeleteExtraReleases([]component.Local{extraRelease}, noConfirm)
			Expect(err).NotTo(HaveOccurred())

			_, err = os.Stat(extraFilePath)
			Expect(os.IsNotExist(err)).To(BeTrue())
		})

		It("sorts the list of releases to be deleted", func() {
			extraReleaseID := component.Spec{Name: "extra-release", Version: "0.0"}
			extraRelease := component.Local{Lock: extraReleaseID.Lock(), LocalPath: extraFilePath}

			zReleaseID := component.Spec{Name: "z-release", Version: "0.0"}
			zRelease := component.Local{Lock: zReleaseID.Lock(), LocalPath: zFilePath}

			result := fmt.Sprintf("- %s\n- %s", extraFilePath, zFilePath)

			err := localReleaseDirectory.DeleteExtraReleases([]component.Local{zRelease, extraRelease}, false)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(logBuf.Contents())).To(ContainSubstring(result))

		})

		Context("when a file cannot be removed", func() {
			It("returns an error", func() {
				extraReleaseID := component.Spec{Name: "extra-release-that-cannot-be-deleted", Version: "0.0"}
				extraRelease := component.Local{Lock: extraReleaseID.Lock(), LocalPath: "file-does-not-exist"}

				err := localReleaseDirectory.DeleteExtraReleases([]component.Local{extraRelease}, noConfirm)
				Expect(err).To(MatchError("failed to delete release extra-release-that-cannot-be-deleted"))
			})
		})
	})
})
