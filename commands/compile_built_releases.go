package commands

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	boshcrypto "github.com/cloudfoundry/bosh-utils/crypto"
	boshsystem "github.com/cloudfoundry/bosh-utils/system"
	"github.com/pivotal-cf/kiln/builder"
	"github.com/pivotal-cf/kiln/helper"
	"github.com/pivotal-cf/kiln/internal/manifest_generator"

	"gopkg.in/src-d/go-billy.v4/osfs"

	"github.com/google/uuid"

	boshdir "github.com/cloudfoundry/bosh-cli/director"
	"github.com/pivotal-cf/jhanda"
	"github.com/pivotal-cf/kiln/fetcher"
	"github.com/pivotal-cf/kiln/internal/cargo"
	"github.com/pivotal-cf/kiln/release"

	boshuaa "github.com/cloudfoundry/bosh-cli/uaa"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
)

type CompileBuiltReleases struct {
	Logger                     *log.Logger
	KilnfileLoader             KilnfileLoader
	MultiReleaseSourceProvider MultiReleaseSourceProvider
	ReleaseUploaderFinder      ReleaseUploaderFinder
	BoshDirectorFactory        func() (BoshDirector, error)

	Options struct {
		ReleasesDir    string `short:"rd" long:"releases-directory" default:"releases" description:"path to a directory to download releases into"`
		StemcellFile   string `short:"sf" long:"stemcell-file"      required:"true"    description:"path to the stemcell tarball on disk"`
		UploadTargetID string `           long:"upload-target-id"   required:"true"    description:"the ID of the release source where the compiled release will be uploaded"`

		Kilnfile       string   `short:"kf" long:"kilnfile"       default:"Kilnfile" description:"path to Kilnfile"`
		VariablesFiles []string `short:"vf" long:"variables-file"                    description:"path to variables file"`
		Variables      []string `short:"vr" long:"variable"                          description:"variable in key=value format"`
	}
}

//go:generate counterfeiter -o ./fakes/bosh_deployment.go --fake-name BoshDeployment github.com/cloudfoundry/bosh-cli/director.Deployment

//go:generate counterfeiter -o ./fakes/bosh_director.go --fake-name BoshDirector . BoshDirector
type BoshDirector interface {
	UploadStemcellFile(file boshdir.UploadFile, fix bool) error
	UploadReleaseFile(file boshdir.UploadFile, rebase, fix bool) error
	FindDeployment(name string) (boshdir.Deployment, error)
	DownloadResourceUnchecked(blobstoreID string, out io.Writer) error
	CleanUp(all bool, dryRun bool, keepOrphanedDisks bool) (boshdir.CleanUp, error)
}

func BoshDirectorFactory() (BoshDirector, error) {
	boshURL := os.Getenv("BOSH_ENVIRONMENT")
	boshClient := os.Getenv("BOSH_CLIENT")
	boshClientSecret := os.Getenv("BOSH_CLIENT_SECRET")
	boshCA := os.Getenv("BOSH_CA_CERT")

	logger := boshlog.NewLogger(boshlog.LevelError)
	factory := boshdir.NewFactory(logger)

	config, err := boshdir.NewConfigFromURL(boshURL)
	if err != nil {
		return nil, err
	}

	config.CACert = boshCA

	basicDirector, err := factory.New(config, boshdir.NewNoopTaskReporter(), boshdir.NewNoopFileReporter())
	if err != nil {
		return nil, err
	}

	info, err := basicDirector.Info()
	if err != nil {
		return nil, fmt.Errorf("could not get basic director info: %s", err)
	}

	uaaClientFactory := boshuaa.NewFactory(logger)

	uaaConfig, err := boshuaa.NewConfigFromURL(info.Auth.Options["url"].(string))
	if err != nil {
		return nil, err
	}

	uaaConfig.Client = boshClient
	uaaConfig.ClientSecret = boshClientSecret
	uaaConfig.CACert = boshCA

	uaa, err := uaaClientFactory.New(uaaConfig)
	if err != nil {
		return nil, fmt.Errorf("could not build uaa auth from director info: %s", err)
	}

	config.TokenFunc = boshuaa.NewClientTokenSession(uaa).TokenFunc

	return factory.New(config, boshdir.NewNoopTaskReporter(), boshdir.NewNoopFileReporter())
}

func (f CompileBuiltReleases) Execute(args []string) error {
	_, err := jhanda.Parse(&f.Options, args)
	if err != nil {
		return err // untested
	}

	f.Logger.Println("loading Kilnfile")
	kilnfile, kilnfileLock, err := f.KilnfileLoader.LoadKilnfiles(osfs.New(""), f.Options.Kilnfile, f.Options.VariablesFiles, f.Options.Variables)
	if err != nil {
		return fmt.Errorf("couldn't load Kilnfiles: %w", err) // untested
	}

	publishableReleaseSources := f.MultiReleaseSourceProvider(kilnfile, true)
	allReleaseSources := f.MultiReleaseSourceProvider(kilnfile, false)
	releaseUploader, err := f.ReleaseUploaderFinder(kilnfile, f.Options.UploadTargetID)
	if err != nil {
		return fmt.Errorf("error loading release uploader: %w", err) // untested
	}

	builtReleases, err := findBuiltReleases(allReleaseSources, kilnfileLock)
	if err != nil {
		return err
	}

	if len(builtReleases) == 0 {
		f.Logger.Println("All releases are compiled. Exiting early")
		return nil
	}

	updatedReleases, remainingBuiltReleases, err := f.downloadPreCompiledReleases(publishableReleaseSources, builtReleases, kilnfileLock.Stemcell)
	if err != nil {
		return err
	}

	if len(remainingBuiltReleases) > 0 {
		f.Logger.Printf("need to compile %d built releases\n", len(remainingBuiltReleases))

		downloadedReleases, stemcell, err := f.compileAndDownloadReleases(allReleaseSources, remainingBuiltReleases)
		if err != nil {
			return err
		}

		uploadedReleases, err := f.uploadCompiledReleases(downloadedReleases, releaseUploader, stemcell)
		if err != nil {
			return err
		}
		updatedReleases = append(updatedReleases, uploadedReleases...)
	} else {
		f.Logger.Println("nothing left to compile")
	}

	err = f.updateLockfile(updatedReleases, kilnfileLock)
	if err != nil {
		return err
	}

	f.Logger.Println("Updated Kilnfile.lock. DONE")
	return nil
}

func (f CompileBuiltReleases) Usage() jhanda.Usage {
	return jhanda.Usage{
		Description:      "Compiles built releases in the Kilnfile.lock and uploads them to the release source",
		ShortDescription: "compiles built releases and uploads them",
		Flags:            f.Options,
	}
}

type remoteReleaseWithSHA1 struct {
	release.Remote
	SHA1 string
}

func findBuiltReleases(allReleaseSources fetcher.MultiReleaseSource, kilnfileLock cargo.KilnfileLock) ([]release.Remote, error) {
	var builtReleases []release.Remote
	for _, lock := range kilnfileLock.Releases {
		src, err := allReleaseSources.FindByID(lock.RemoteSource)
		if err != nil {
			return nil, err
		}
		if !src.Publishable() {
			releaseID := release.ID{Name: lock.Name, Version: lock.Version}
			builtReleases = append(builtReleases, release.Remote{
				ID:         releaseID,
				SourceID:   lock.RemoteSource,
				RemotePath: lock.RemotePath,
			})
		}
	}
	return builtReleases, nil
}

func (f CompileBuiltReleases) downloadPreCompiledReleases(publishableReleaseSources fetcher.MultiReleaseSource, builtReleases []release.Remote, stemcell cargo.Stemcell) ([]remoteReleaseWithSHA1, []release.Remote, error) {
	var (
		remainingBuiltReleases []release.Remote
		preCompiledReleases    []remoteReleaseWithSHA1
	)

	f.Logger.Println("searching for pre-compiled releases")

	for _, builtRelease := range builtReleases {
		spec := release.Requirement{
			Name:            builtRelease.Name,
			Version:         builtRelease.Version,
			StemcellOS:      stemcell.OS,
			StemcellVersion: stemcell.Version,
		}
		remote, found, err := publishableReleaseSources.GetMatchedRelease(spec)
		if err != nil {
			return nil, nil, fmt.Errorf("error searching for pre-compiled release for %q: %w", builtRelease.Name, err)
		}
		if !found {
			remainingBuiltReleases = append(remainingBuiltReleases, builtRelease)
			continue
		}

		local, err := publishableReleaseSources.DownloadRelease(f.Options.ReleasesDir, remote, fetcher.DefaultDownloadThreadCount)
		if err != nil {
			return nil, nil, fmt.Errorf("error downloading pre-compiled release for %q: %w", builtRelease.Name, err)
		}

		preCompiledReleases = append(preCompiledReleases, remoteReleaseWithSHA1{Remote: remote, SHA1: local.SHA1})
	}

	f.Logger.Printf("found %d pre-compiled releases\n", len(preCompiledReleases))

	return preCompiledReleases, remainingBuiltReleases, nil
}

func (f CompileBuiltReleases) compileAndDownloadReleases(releaseSource fetcher.MultiReleaseSource, builtReleases []release.Remote) ([]release.Local, builder.StemcellManifest, error) {
	f.Logger.Println("connecting to the bosh director")
	boshDirector, err := f.BoshDirectorFactory()
	if err != nil {
		return nil, builder.StemcellManifest{}, fmt.Errorf("unable to connect to bosh director: %w", err) // untested
	}

	releaseIDs, err := f.uploadReleasesToDirector(builtReleases, releaseSource, boshDirector)
	if err != nil {
		return nil, builder.StemcellManifest{}, err
	}

	stemcellManifest, err := f.uploadStemcellToDirector(boshDirector)
	if err != nil {
		return nil, builder.StemcellManifest{}, err
	}

	deploymentName := fmt.Sprintf("compile-built-releases-%s", uuid.Must(uuid.NewRandom()))
	f.Logger.Printf("deploying compilation deployment %q\n", deploymentName)
	deployment, err := boshDirector.FindDeployment(deploymentName)
	if err != nil {
		return nil, builder.StemcellManifest{}, fmt.Errorf("couldn't create deployment: %w", err) // untested
	}

	mg := manifest_generator.NewManifestGenerator()
	manifest, err := mg.Generate(deploymentName, releaseIDs, stemcellManifest)
	if err != nil {
		return nil, builder.StemcellManifest{}, fmt.Errorf("couldn't generate bosh manifest: %v", err) // untested
	}

	err = deployment.Update(manifest, boshdir.UpdateOpts{})
	if err != nil {
		return nil, builder.StemcellManifest{}, fmt.Errorf("updating the bosh deployment: %v", err) // untested
	}

	defer func() {
		f.Logger.Println("deleting compilation deployment")
		err = deployment.Delete(true)
		if err != nil {
			panic(fmt.Errorf("error deleting the deployment: %w", err))
		}

		f.Logger.Println("cleaning up unused releases and stemcells")
		_, err = boshDirector.CleanUp(true, false, false)
		if err != nil {
			f.Logger.Println(fmt.Sprintf("warning: bosh director failed cleanup with the following error: %v", err))
			return
		}
	}()

	downloadedReleases, err := f.downloadCompiledReleases(stemcellManifest, releaseIDs, deployment, boshDirector)
	if err != nil {
		return nil, builder.StemcellManifest{}, err // untested
	}

	return downloadedReleases, stemcellManifest, nil
}

func (f CompileBuiltReleases) uploadReleasesToDirector(builtReleases []release.Remote, releaseSource fetcher.MultiReleaseSource, boshDirector BoshDirector) ([]release.ID, error) {
	var releaseIDs []release.ID
	for _, remoteRelease := range builtReleases {
		releaseIDs = append(releaseIDs, remoteRelease.ID)

		localRelease, err := releaseSource.DownloadRelease(f.Options.ReleasesDir, remoteRelease, fetcher.DefaultDownloadThreadCount)
		if err != nil {
			return nil, fmt.Errorf("failure downloading built release %v: %w", remoteRelease.ID, err) // untested
		}

		builtReleaseForUploading, err := os.Open(localRelease.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("opening local built release %q: %w", localRelease.LocalPath, err) // untested
		}

		f.Logger.Printf("uploading release %q to director\n", localRelease.LocalPath)
		err = boshDirector.UploadReleaseFile(builtReleaseForUploading, false, false)
		if err != nil {
			return nil, fmt.Errorf("failure uploading release %q to bosh director: %w", localRelease.LocalPath, err) // untested
		}
	}
	return releaseIDs, nil
}

func (f CompileBuiltReleases) uploadStemcellToDirector(boshDirector BoshDirector) (builder.StemcellManifest, error) {
	f.Logger.Printf("uploading stemcell %q to director\n", f.Options.StemcellFile)
	stemcellFile, err := os.Open(f.Options.StemcellFile)
	if err != nil {
		return builder.StemcellManifest{}, fmt.Errorf("opening stemcell: %w", err) // untested
	}

	err = boshDirector.UploadStemcellFile(stemcellFile, false)
	if err != nil {
		return builder.StemcellManifest{}, fmt.Errorf("failure uploading stemcell to bosh director: %w", err) // untested
	}

	stemcellManifestReader := builder.NewStemcellManifestReader(helper.NewFilesystem())
	stemcellPart, err := stemcellManifestReader.Read(f.Options.StemcellFile)
	if err != nil {
		return builder.StemcellManifest{}, fmt.Errorf("couldn't parse manifest of stemcell: %v", err) // untested
	}

	stemcellManifest := stemcellPart.Metadata.(builder.StemcellManifest)
	return stemcellManifest, err
}

func (f CompileBuiltReleases) downloadCompiledReleases(stemcellManifest builder.StemcellManifest, releaseIDs []release.ID, deployment boshdir.Deployment, boshDirector BoshDirector) ([]release.Local, error) {
	osVersionSlug := boshdir.NewOSVersionSlug(stemcellManifest.OperatingSystem, stemcellManifest.Version)
	var downloadedReleases []release.Local
	for _, rel := range releaseIDs {
		compiledTarballPath := filepath.Join(f.Options.ReleasesDir, fmt.Sprintf("%s-%s-%s-%s.tgz", rel.Name, rel.Version, stemcellManifest.OperatingSystem, stemcellManifest.Version))
		f.Logger.Printf("exporting release %q\n", compiledTarballPath)

		result, err := deployment.ExportRelease(boshdir.NewReleaseSlug(rel.Name, rel.Version), osVersionSlug, nil)
		if err != nil {
			return nil, fmt.Errorf("exporting release %s: %w", rel.Name, err)
		}

		fd, err := os.OpenFile(compiledTarballPath, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return nil, fmt.Errorf("creating compiled release file %s: %w", compiledTarballPath, err) // untested
		}

		f.Logger.Printf("downloading release %q from director\n", rel.Name)
		err = boshDirector.DownloadResourceUnchecked(result.BlobstoreID, fd)
		if err != nil {
			return nil, fmt.Errorf("downloading exported release %s: %w", rel.Name, err)
		}

		err = fd.Close()
		if err != nil {
			return nil, fmt.Errorf("failed closing file %s: %w", compiledTarballPath, err) // untested
		}

		fd, err = os.Open(compiledTarballPath)
		if err != nil {
			return nil, fmt.Errorf("failed reopening file %s: %w", compiledTarballPath, err) // untested
		}

		s := sha1.New()
		_, err = io.Copy(s, fd)
		if err != nil {
			return nil, fmt.Errorf("failed calculating SHA1 for file file %s: %w", compiledTarballPath, err) // untested
		}
		err = fd.Close()
		if err != nil {
			return nil, fmt.Errorf("failed closing file %s: %w", compiledTarballPath, err) // untested
		}

		downloadedReleases = append(downloadedReleases, release.Local{
			ID:        release.ID{Name: rel.Name, Version: rel.Version},
			LocalPath: compiledTarballPath,
			SHA1:      hex.EncodeToString(s.Sum(nil)),
		})

		expectedMultipleDigest, err := boshcrypto.ParseMultipleDigest(result.SHA1)
		if err != nil {
			return nil, fmt.Errorf("error parsing SHA of downloaded release %q: %w", rel.Name, err) // untested
		}

		ignoreMeLogger := boshlog.NewLogger(boshlog.LevelNone)
		fs := boshsystem.NewOsFileSystem(ignoreMeLogger)
		err = expectedMultipleDigest.VerifyFilePath(compiledTarballPath, fs)
		if err != nil {
			return nil, fmt.Errorf("compiled release %q has an incorrect SHA: %w", rel.Name, err)
		}

	}
	return downloadedReleases, nil
}

func (f CompileBuiltReleases) uploadCompiledReleases(downloadedReleases []release.Local, releaseUploader fetcher.ReleaseUploader, stemcell builder.StemcellManifest) ([]remoteReleaseWithSHA1, error) {
	var uploadedReleases []remoteReleaseWithSHA1

	for _, downloadedRelease := range downloadedReleases {
		releaseFile, err := os.Open(downloadedRelease.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("opening compiled release %q for uploading: %w", downloadedRelease.LocalPath, err) // untested
		}

		remoteRelease, err := releaseUploader.UploadRelease(release.Requirement{
			Name:            downloadedRelease.Name,
			Version:         downloadedRelease.Version,
			StemcellOS:      stemcell.OperatingSystem,
			StemcellVersion: stemcell.Version,
		}, releaseFile)
		if err != nil {
			return nil, fmt.Errorf("uploading compiled release %q failed: %w", downloadedRelease.LocalPath, err) // untested
		}

		uploadedReleases = append(uploadedReleases, remoteReleaseWithSHA1{Remote: remoteRelease, SHA1: downloadedRelease.SHA1})
	}
	return uploadedReleases, nil
}

func (f CompileBuiltReleases) updateLockfile(uploadedReleases []remoteReleaseWithSHA1, kilnfileLock cargo.KilnfileLock) error {
	for _, uploaded := range uploadedReleases {
		var matchingRelease *cargo.ReleaseLock
		for i := range kilnfileLock.Releases {
			if kilnfileLock.Releases[i].Name == uploaded.Name {
				matchingRelease = &kilnfileLock.Releases[i]
				break
			}
		}
		if matchingRelease == nil {
			return fmt.Errorf("no release named %q exists in your Kilnfile.lock", uploaded.Name) // untested (shouldn't be possible)
		}

		matchingRelease.RemoteSource = uploaded.SourceID
		matchingRelease.RemotePath = uploaded.RemotePath
		matchingRelease.SHA1 = uploaded.SHA1
	}

	return f.KilnfileLoader.SaveKilnfileLock(osfs.New(""), f.Options.Kilnfile, kilnfileLock)
}
