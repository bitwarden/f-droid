package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"metascoop/apps"
	"metascoop/file"
	"metascoop/git"
	"metascoop/md"

	"github.com/google/go-github/v39/github"
	"golang.org/x/oauth2"
)

func main() {
	var (
		reposFilePath = flag.String("rp", "repos.yaml", "Path to repos.yaml file")
		repoDir       = flag.String("rd", "fdroid/repo", "Path to fdroid \"repo\" directory")
		accessToken   = flag.String("pat", "", "GitHub personal access token")

		debugMode = flag.Bool("debug", false, "Debug mode won't run the fdroid command")
	)
	flag.Parse()

	fmt.Println("::group::Initializing")

	reposList, err := apps.ParseRepoFile(*reposFilePath)
	if err != nil {
		log.Fatalf("parsing given repos file: %s\n", err.Error())
	}

	var authenticatedClient *http.Client
	if *accessToken != "" {
		ctx := context.Background()
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: *accessToken},
		)
		authenticatedClient = oauth2.NewClient(ctx, ts)
	}
	githubClient := github.NewClient(authenticatedClient)

	fdroidIndexFilePath := filepath.Join(*repoDir, "index-v1.json")

	initialFdroidIndex, err := apps.ReadIndex(fdroidIndexFilePath)
	if err != nil {
		log.Fatalf("reading f-droid repo index: %s\n", err.Error())
	}

	err = os.MkdirAll(*repoDir, 0o644)
	if err != nil {
		log.Fatalf("creating repo directory: %s\n", err.Error())
	}

	fmt.Println("::endgroup::Initializing")

	var (
		haveError          bool
		apkInfoMap         = make(map[string]apps.Application)
		apkInfoMapMutex    sync.Mutex
		toRemovePaths      []string
		toRemovePathsMutex sync.Mutex
	)

	for _, repo := range reposList {
		fmt.Printf("::group::Repo: %s/%s\n", repo.Author, repo.Identifier)

		err, releases := getRepositoryReleases(githubClient, repo)
		if err != nil {
			log.Printf("Error while listing repo releases for %q: %s\n", repo.GitURL, err.Error())
			haveError = true
			return
		}

		log.Printf("Received %d releases", len(releases))

		for _, app := range repo.Applications {
			fmt.Printf("::group::App %s\n", app.Name)

			downloaded := false

			for _, release := range releases {
				fmt.Printf("::group::Release %s\n", release.GetTagName())

				if release.GetDraft() {
					log.Printf("Skipping draft %q\n", release.GetTagName())
					continue
				}
				if release.GetTagName() == "" {
					log.Printf("Skipping release with empty tag name")
					continue
				}

				log.Printf("Working on release with tag name %q", release.GetTagName())
				var apk *github.ReleaseAsset = apps.FindAPK(release, app.Filename)

				if apk == nil {
					log.Printf("Couldn't find any F-Droid assets for application %s in %s with file name %s", app.Filename, release.GetName(), app.Filename)
					continue
				}

				appName := apps.GenerateReleaseFilename(app.Id, release.GetTagName())

				log.Printf("Target APK name: %s\n", appName)

				appClone := app

				appClone.ReleaseDescription = release.GetBody()
				if appClone.ReleaseDescription != "" {
					log.Printf("Release notes: \n%s\n", appClone.ReleaseDescription)
				}

				apkInfoMapMutex.Lock()
				apkInfoMap[appName] = appClone
				apkInfoMapMutex.Unlock()

				appTargetPath := filepath.Join(*repoDir, appName)

				// If the app file already exists for this version, we stop processing this app and move to the next
				if _, err := os.Stat(appTargetPath); !errors.Is(err, os.ErrNotExist) {
					log.Printf("Already have APK for version %q at %q\n", release.GetTagName(), appTargetPath)
					downloaded = true
					break
				}

				log.Printf("Downloading APK %q from release %q to %q", apk.GetName(), release.GetTagName(), appTargetPath)

				downloadContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				appStream, _, err := githubClient.Repositories.DownloadReleaseAsset(downloadContext, repo.Author, repo.Identifier, apk.GetID(), http.DefaultClient)
				if err != nil {
					log.Printf("Error while downloading app %q (artifact id %d) from from release %q: %s", repo.GitURL, apk.GetID(), release.GetTagName(), err.Error())
					haveError = true
					break
				}

				err = downloadStream(appTargetPath, appStream)
				if err != nil {
					log.Printf("Error while downloading app %q (artifact id %d) from from release %q to %q: %s", repo.GitURL, *apk.ID, *release.TagName, appTargetPath, err.Error())
					haveError = true
					break
				}

				log.Printf("Successfully downloaded app for version %q", release.GetTagName())
				fmt.Printf("::endgroup:App %s\n", app.Name)
				break

			}
			if downloaded || haveError {
				// Stop after the first [release] of this [app] is downloaded to prevent back-filling legacy releases.
				break
			}
		}

		fmt.Printf("::endgroup::Repo: %s/%s\n", repo.Author, repo.Identifier)
	}

	if haveError {
		os.Exit(1)
	}

	if !*debugMode {
		fmt.Println("::group::F-Droid: Creating metadata stubs")
		// Now, we run the fdroid update command
		cmd := exec.Command("fdroid", "update", "--pretty", "--create-metadata", "--delete-unknown")
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		cmd.Stdin = os.Stdin
		cmd.Dir = filepath.Dir(*repoDir)

		log.Printf("Running %q in %s", cmd.String(), cmd.Dir)

		err = cmd.Run()

		if err != nil {
			log.Println("Error while running \"fdroid update -c\":", err.Error())

			fmt.Println("::endgroup::F-Droid: Creating metadata stubs")
			os.Exit(1)
		}
		fmt.Println("::endgroup::F-Droid Creating metadata stubs")
	}

	fmt.Println("Filling in metadata")

	fdroidIndex, err := apps.ReadIndex(fdroidIndexFilePath)
	if err != nil {
		log.Fatalf("reading f-droid repo index: %s\n::endgroup::\n", err.Error())
	}

	walkPath := filepath.Join(filepath.Dir(*repoDir), "metadata")
	err = filepath.WalkDir(walkPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".yml") {
			return err
		}

		pkgname := strings.TrimSuffix(filepath.Base(path), ".yml")

		fmt.Printf("::group::Package %s\n", pkgname)

		return func() error {
			defer fmt.Printf("::endgroup::Package %s\n", pkgname)
			log.Printf("Working on %q", pkgname)

			meta, err := apps.ReadMetaFile(path)
			if err != nil {
				log.Printf("Reading meta file %q: %s", path, err.Error())
				return nil
			}

			latestPackage, ok := fdroidIndex.FindLatestPackage(pkgname)
			if !ok {
				return nil
			}

			log.Printf("The latest version is %q with versionCode %d", latestPackage.VersionName, latestPackage.VersionCode)

			apkInfo, ok := apkInfoMap[latestPackage.ApkName]
			if !ok {
				log.Printf("Cannot find apk info for %q", latestPackage.ApkName)
				return nil
			}

			// Now update with some info
			for _, repo := range reposList {
				if repoHasApp(repo, latestPackage.PackageName) {
					setNonEmpty(meta, "AuthorName", repo.Author)
					setNonEmpty(meta, "License", repo.License)

					summary := repo.Summary
					// See https://f-droid.org/en/docs/Build_Metadata_Reference/#Summary for max length
					const maxSummaryLength = 80
					if len(summary) > maxSummaryLength {
						summary = summary[:maxSummaryLength-3] + "..."

						log.Printf("Truncated summary to length of %d (max length)", len(summary))
					}
					setNonEmpty(meta, "Summary", summary)
					break // Found the repo, no need to continue
				}
			}

			fn := apkInfo.Name
			if fn == "" {
				fn = apkInfo.Id
			}
			setNonEmpty(meta, "Name", fn)
			setNonEmpty(meta, "SourceCode", apkInfo.GitURL)
			setNonEmpty(meta, "Description", apkInfo.Description)

			if len(apkInfo.Categories) != 0 {
				meta["Categories"] = apkInfo.Categories
			}

			if len(apkInfo.AntiFeatures) != 0 {
				meta["AntiFeatures"] = strings.Join(apkInfo.AntiFeatures, ",")
			}

			meta["CurrentVersion"] = latestPackage.VersionName
			meta["CurrentVersionCode"] = latestPackage.VersionCode

			log.Printf("Set current version info to versionName=%q, versionCode=%d", latestPackage.VersionName, latestPackage.VersionCode)

			err = apps.WriteMetaFile(path, meta)
			if err != nil {
				log.Printf("Writing meta file %q: %s", path, err.Error())
				return nil
			}

			log.Printf("Updated metadata file %q", path)

			if apkInfo.ReleaseDescription != "" {
				destFilePath := filepath.Join(walkPath, latestPackage.PackageName, "en-US", "changelogs", fmt.Sprintf("%d.txt", latestPackage.VersionCode))

				err = os.MkdirAll(filepath.Dir(destFilePath), os.ModePerm)
				if err != nil {
					log.Printf("Creating directory for changelog file %q: %s", destFilePath, err.Error())
					return nil
				}

				err = os.WriteFile(destFilePath, []byte(apkInfo.ReleaseDescription), os.ModePerm)
				if err != nil {
					log.Printf("Writing changelog file %q: %s", destFilePath, err.Error())
					return nil
				}

				log.Printf("Wrote release notes to %q", destFilePath)
			}

			// Find the repo for this package
			var repoForPackage *apps.Repo
			for _, repo := range reposList {
				if repoHasApp(repo, latestPackage.PackageName) {
					repoForPackage = &repo
					break
				}
			}
			if repoForPackage == nil {
				log.Printf("Could not find repo for package %s", latestPackage.PackageName)
				return nil
			}

			log.Printf("Cloning git repository to search for screenshots")

			gitRepoPath, err := git.CloneRepo(repoForPackage.GitURL)
			if err != nil {
				log.Printf("Cloning git repo from %q: %s", repoForPackage.GitURL, err.Error())
				return nil
			}
			defer os.RemoveAll(gitRepoPath)

			metadata, err := apps.FindMetadata(gitRepoPath)
			if err != nil {
				log.Printf("finding metadata in git repo %q: %s", gitRepoPath, err.Error())
				return nil
			}

			log.Printf("Found %d screenshots", len(metadata.Screenshots))

			screenshotsPath := filepath.Join(walkPath, latestPackage.PackageName, "en-US", "phoneScreenshots")

			_ = os.RemoveAll(screenshotsPath)

			var sccounter int = 1
			for _, sc := range metadata.Screenshots {
				var ext = filepath.Ext(sc)
				if ext == "" {
					log.Printf("Invalid: screenshot file extension is empty for %q", sc)
					continue
				}

				var newFilePath = filepath.Join(screenshotsPath, fmt.Sprintf("%d%s", sccounter, ext))

				err = os.MkdirAll(filepath.Dir(newFilePath), os.ModePerm)
				if err != nil {
					log.Printf("Creating directory for screenshot file %q: %s", newFilePath, err.Error())
					return nil
				}

				err = file.Move(sc, newFilePath)
				if err != nil {
					log.Printf("Moving screenshot file %q to %q: %s", sc, newFilePath, err.Error())
					return nil
				}

				log.Printf("Wrote screenshot to %s", newFilePath)

				sccounter++
			}

			toRemovePathsMutex.Lock()
			toRemovePaths = append(toRemovePaths, screenshotsPath)
			toRemovePathsMutex.Unlock()

			return nil
		}()
	})

	if err != nil {
		log.Printf("Error while walking metadata: %s", err.Error())

		os.Exit(1)
	}

	if !*debugMode {
		fmt.Println("::group::F-Droid: Reading updated metadata")

		// Now, we run the fdroid update command again to regenerate the index with our new metadata
		cmd := exec.Command("fdroid", "update", "--pretty", "--delete-unknown")
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		cmd.Stdin = os.Stdin
		cmd.Dir = filepath.Dir(*repoDir)

		log.Printf("Running %q in %s", cmd.String(), cmd.Dir)

		err = cmd.Run()
		if err != nil {
			log.Println("Error while running \"fdroid update -c\":", err.Error())

			fmt.Println("::endgroup::F-Droid: Reading updated metadata")
			os.Exit(1)
		}
		fmt.Println("::endgroup::F-Droid: Reading updated metadata")
	}

	fmt.Println("::group::Assessing changes")

	// Now at the end, we read the index again
	fdroidIndex, err = apps.ReadIndex(fdroidIndexFilePath)
	if err != nil {
		log.Fatalf("reading f-droid repo index: %s\n::endgroup::\n", err.Error())
	}

	// Now we can remove all paths that were marked for doing so

	for _, rmpath := range toRemovePaths {
		err = os.RemoveAll(rmpath)
		if err != nil {
			log.Fatalf("removing path %q: %s\n", rmpath, err.Error())
		}
	}

	// We can now generate the README file
	readmePath := filepath.Join(filepath.Dir(filepath.Dir(*repoDir)), "README.md")
	err = md.RegenerateReadme(readmePath, fdroidIndex)
	if err != nil {
		log.Fatalf("error generating %q: %s\n", readmePath, err.Error())
	}

	cpath, haveSignificantChanges := apps.HasSignificantChanges(initialFdroidIndex, fdroidIndex)
	if haveSignificantChanges {
		log.Printf("The index %q had a significant change at JSON path %q", fdroidIndexFilePath, cpath)
	} else {
		log.Printf("The index files didn't change significantly")

		changedFiles, err := git.GetChangedFileNames(*repoDir)
		if err != nil {
			log.Fatalf("getting changed files: %s\n::endgroup::\n", err.Error())
		}

		// If only the index files changed, we ignore the commit
		for _, fname := range changedFiles {
			if !strings.Contains(fname, "index") {
				haveSignificantChanges = true

				log.Printf("File %q is a significant change", fname)
			}
		}

		if !haveSignificantChanges {
			log.Printf("It doesn't look like there were any relevant changes, neither to the index file nor any file indexed by git.")
		}
	}

	fmt.Println("::endgroup::Assessing changes")

	// If we don't have any good changes, we report it with exit code 2
	if !haveSignificantChanges {
		os.Exit(2)
	}

	// If we have relevant changes, we exit with code 0
}

func getRepositoryReleases(githubClient *github.Client, repo apps.Repo) (error, []*github.RepositoryRelease) {
	log.Printf("Looking up %s/%s on GitHub", repo.Author, repo.Identifier)
	gitHubRepo, _, err := githubClient.Repositories.Get(context.Background(), repo.Author, repo.Identifier)
	if err != nil {
		log.Printf("Error while looking up repo: %s", err.Error())
	} else {
		repo.Summary = gitHubRepo.GetDescription()

		if gitHubRepo.License != nil && gitHubRepo.License.SPDXID != nil {
			repo.License = *gitHubRepo.License.SPDXID
		}

		log.Printf("Data from GitHub: summary=%q, license=%q", repo.Summary, repo.License)
	}

	releases, err := apps.ListAllReleases(githubClient, repo.Author, repo.Identifier)
	return err, releases
}

func setNonEmpty(m map[string]interface{}, key string, value string) {
	if value != "" || m[key] == "Unknown" {
		m[key] = value

		log.Printf("Set %s to %q", key, value)
	}
}

func downloadStream(targetFile string, rc io.ReadCloser) (err error) {
	defer rc.Close()

	targetTemp := targetFile + ".tmp"

	f, err := os.Create(targetTemp)
	if err != nil {
		return
	}

	_, err = io.Copy(f, rc)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(targetTemp)

		return
	}

	err = f.Close()
	if err != nil {
		return
	}

	return os.Rename(targetTemp, targetFile)
}

func repoHasApp(repo apps.Repo, packageName string) bool {
	for _, app := range repo.Applications {
		if app.Id == packageName {
			return true
		}
	}
	return false
}
