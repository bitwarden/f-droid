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

	"github.com/google/go-github/v79/github"
	"golang.org/x/oauth2"
)

func main() {
	var (
		reposFilePath = flag.String("rp", "repos.yaml", "Path to repos.yaml file")
		repoDir       = flag.String("rd", "fdroid/repo", "Path to fdroid \"repo\" directory")
		accessToken   = flag.String("pat", "", "GitHub personal access token")
		commitMsgFile = flag.String("cm", "commit_message.tmp", "Path to the commit message file")
		debugMode     = flag.Bool("debug", false, "Debug mode won't run the fdroid command")
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
		toRemovePaths      []string
		repoAppReleasesMap = make(map[string]map[*apps.Application][]*github.RepositoryRelease)
		mu                 sync.Mutex
		wg                 sync.WaitGroup
	)

	isCommittingNewReleases := false

	for _, repo := range reposList {
		wg.Add(1)
		go func(repo apps.Repo) {
			defer wg.Done()

			fmt.Printf("::group::Repo: %s/%s\n", repo.Owner, repo.Name)

			err, releases := getRepositoryReleases(githubClient, repo)
			if err != nil {
				log.Printf("Error while listing repo releases for %q: %s\n", repo.GitURL, err.Error())
				mu.Lock()
				haveError = true
				mu.Unlock()
				return
			}

			log.Printf("Received %d releases", len(releases))

			var appWg sync.WaitGroup
			for _, app := range repo.Applications {
				appWg.Add(1)
				go func(app apps.Application) {
					defer appWg.Done()

					fmt.Printf("::group::App %s\n", app.Name)

					for i, release := range releases {
						if i >= 5 {
							log.Printf("5 latest releases have been processed for app %s, skipping the rest.", app.Name)
							break
						}
						fmt.Printf("::group::Release %s\n", release.GetTagName())

						if release.GetDraft() {
							log.Printf("Skipping draft %q\n", release.GetTagName())
							continue
						}
						if release.GetPrerelease() {
							log.Printf("Skipping pre-release %q\n", release.GetTagName())
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

						mu.Lock()
						apkInfoMap[appName] = appClone
						mu.Unlock()

						appTargetPath := filepath.Join(*repoDir, appName)

						// If the app file already exists for this version, we skip processing this release and move to the next
						if _, err := os.Stat(appTargetPath); !errors.Is(err, os.ErrNotExist) {
							log.Printf("Already have APK for version %q at %q\n", release.GetTagName(), appTargetPath)
							continue
						}

						log.Printf("Downloading APK %q from release %q to %q", apk.GetName(), release.GetTagName(), appTargetPath)

						downloadContext, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancel()

						appStream, _, err := githubClient.Repositories.DownloadReleaseAsset(downloadContext, repo.Owner, repo.Name, apk.GetID(), http.DefaultClient)
						if err != nil {
							log.Printf("Error while downloading app %q (artifact id %d) from from release %q: %s", repo.GitURL, apk.GetID(), release.GetTagName(), err.Error())
							mu.Lock()
							haveError = true
							mu.Unlock()
							break
						}

						err = downloadStream(appTargetPath, appStream)
						if err != nil {
							log.Printf("Error while downloading app %q (artifact id %d) from from release %q to %q: %s", repo.GitURL, *apk.ID, *release.TagName, appTargetPath, err.Error())
							mu.Lock()
							haveError = true
							mu.Unlock()
							break
						}

						log.Printf("Successfully downloaded app for version %q", release.GetTagName())
						fmt.Printf("::endgroup:App %s\n", app.Name)
						mu.Lock()
						isCommittingNewReleases = true

						if repoAppReleasesMap[repo.GitURL] == nil {
							repoAppReleasesMap[repo.GitURL] = make(map[*apps.Application][]*github.RepositoryRelease)
						}
						repoAppReleasesMap[repo.GitURL][&app] = append(repoAppReleasesMap[repo.GitURL][&app], release)
						mu.Unlock()
					}

					if haveError {
						return
					}
				}(app)
			}

			appWg.Wait()

			if isCommittingNewReleases {
				log.Printf("Changes detected for repo: %s", repo.GitURL)
			}

			fmt.Printf("::endgroup::Repo: %s/%s\n", repo.Owner, repo.Name)
		}(repo)
	}

	wg.Wait()

	var commitMsg strings.Builder
	if isCommittingNewReleases {
		log.Printf("New commits detected in at least one repo. Creating commit message with application update details.")
		// Create the commit title with modified repo names. E.g., "Update apps from repo1, repo2".
		repoNames := make([]string, 0, len(repoAppReleasesMap))
		for repoURL := range repoAppReleasesMap {
			repoName := strings.TrimPrefix(repoURL, "https://github.com/")
			repoNames = append(repoNames, repoName)
		}
		commitMsg.WriteString(fmt.Sprintf("Updated apps from %s", strings.Join(repoNames, ", ")))

		// Add newlines before starting commit body
		commitMsg.WriteString("\n\n")

		// Add header for updated repositories
		commitMsg.WriteString("## Updated repositories\n")

		for repoURL, appReleasesMap := range repoAppReleasesMap {

			// Add a h2 header for each unique repo with changes. E.g., "## [bitwarden/android](https://github.com/bitawrden/android)".
			repoFullName := strings.TrimPrefix(repoURL, "https://github.com/")
			commitMsg.WriteString(fmt.Sprintf("## [%s](%s)\n", repoFullName, repoURL))

			// Add h3 header for apps section.
			commitMsg.WriteString("### Apps\n")

			// Add details section for each app. E.g., "## Bitwarden" or "## Bitwarden Beta".
			for application, appReleases := range appReleasesMap {
				commitMsg.WriteString(fmt.Sprintf("####  %s\n\n", application.Name))

				// Add links to each release of the app. E.g., "- [v2024.11.0](https://github.com/bitwarden/android/releases/v2024.11.0)".
				for i, appRelease := range appReleases {
					releaseName := appRelease.GetName()
					if releaseName == "" {
						releaseName = appRelease.GetTagName()
					}
					releaseTagUrl := appRelease.GetHTMLURL()
					commitMsg.WriteString(fmt.Sprintf("  - [%s](%s)", releaseName, releaseTagUrl))

					// Append "latest" if this is the newest release
					if i == 0 {
						commitMsg.WriteString(" **latest**")
					}
					commitMsg.WriteString("\n")
				}
			}
		}
	} else {
		log.Printf("No new releases detected.")
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
					setNonEmpty(meta, "AuthorName", repo.Owner)
					setNonEmpty(meta, "License", repo.License)
					setNonEmpty(meta, "SourceCode", repo.GitURL)

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

			toRemovePaths = append(toRemovePaths, screenshotsPath)

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

	cpath, hasSignificantChanges := apps.HasSignificantChanges(initialFdroidIndex, fdroidIndex)
	if hasSignificantChanges {
		log.Printf("The index %q had a significant change at JSON path %q", fdroidIndexFilePath, cpath)
		// If there were no new releases, we add a commit title indicating the index has changes.
		if !isCommittingNewReleases {
			commitMsg.WriteString("Automatic index update\n\n")
		}
	} else {
		log.Printf("The index files didn't change significantly")
		if !isCommittingNewReleases {
			log.Printf("No new releases detected either. Exiting with code 2.")
			os.Exit(2)
		}
	}

	changedFiles, err := git.GetChangedFileNames(*repoDir)
	if err != nil {
		log.Fatalf("getting changed files: %s\n::endgroup::\n", err.Error())
	}

	// If only the index files changed, we ignore the commit
	var modifiedFiles []string
	for _, fname := range changedFiles {
		if !strings.Contains(fname, "index") {
			hasSignificantChanges = true
			modifiedFiles = append(modifiedFiles, fname)
			log.Printf("File %q is a significant change", fname)
		}
	}

	// If there were modified files, we add them to the commit message
	if len(modifiedFiles) > 0 {

		// If there were new releases, we add a header to separate metadata changes.
		if isCommittingNewReleases {
			commitMsg.WriteString("## Metadata updates:\n\n")
		}
		commitMsg.WriteString("We performed updates to repository metadata files.\n")
		commitMsg.WriteString("<details>\n<summary>See what changed</summary>\n\n")
		for _, fname := range modifiedFiles {
			commitMsg.WriteString(fmt.Sprintf("  - %s\n", fname))
		}
		commitMsg.WriteString("</details>\n")
	}

	if haveError {
		os.Exit(1)
	}

	fmt.Println("::endgroup::Assessing changes")

	// If we don't have any significant changes, we report it with exit code 2.
	if !hasSignificantChanges {
		os.Exit(2)
	}

	// Otherwise, we write the commit message and exit with code 0.

	// Create a temporary commit message file.
	tempFile, err := os.Create(*commitMsgFile)
	if err != nil {
		log.Fatalf("Error creating commit message file: %v", err)
	}
	defer tempFile.Close()
	log.Printf("Commit message file created: %s", *commitMsgFile)

	// Write the commit message to the file.
	_, err = tempFile.WriteString(commitMsg.String())
	if err != nil {
		log.Printf("Error writing commit message file: %s", err)
	} else {
		log.Printf("Commit message written to %s\n%s", *commitMsgFile, commitMsg.String())
	}
}

func getRepositoryReleases(githubClient *github.Client, repo apps.Repo) (error, []*github.RepositoryRelease) {
	log.Printf("Looking up %s/%s on GitHub", repo.Owner, repo.Name)
	gitHubRepo, _, err := githubClient.Repositories.Get(context.Background(), repo.Owner, repo.Name)
	if err != nil {
		log.Printf("Error while looking up repo: %s", err.Error())
	} else {
		repo.Summary = gitHubRepo.GetDescription()

		if gitHubRepo.License != nil && gitHubRepo.License.SPDXID != nil {
			repo.License = *gitHubRepo.License.SPDXID
		}

		log.Printf("Data from GitHub: summary=%q, license=%q", repo.Summary, repo.License)
	}

	releases, err := apps.ListAllReleases(githubClient, repo.Owner, repo.Name)
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
