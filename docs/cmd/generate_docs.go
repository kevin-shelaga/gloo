package main

import (
	"context"
	"fmt"
	"net/http"
	"io/ioutil"
	"os"
	"regexp"

	"github.com/Masterminds/semver/v3"
	"github.com/google/go-github/v32/github"
	"github.com/rotisserie/eris"
	. "github.com/solo-io/gloo/docs/cmd/securityscanutils"
	"github.com/solo-io/go-utils/changelogutils/changelogdocutils"
	"github.com/solo-io/go-utils/githubutils"
	. "github.com/solo-io/go-utils/versionutils"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
)

func main() {
	ctx := context.Background()
	app := rootApp(ctx)
	if err := app.Execute(); err != nil {
		fmt.Printf("unable to run: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	ctx              context.Context
	HugoDataSoloOpts HugoDataSoloOpts
}

type HugoDataSoloOpts struct {
	product string
	version string
	// if set, will override the version when rendering the
	callLatest bool
	noScope    bool
}

func rootApp(ctx context.Context) *cobra.Command {
	opts := &options{
		ctx: ctx,
	}
	app := &cobra.Command{
		Use: "docs-util",
		RunE: func(cmd *cobra.Command, args []string) error {

			return nil
		},
	}
	app.AddCommand(changelogMdFromGithubCmd(opts))
	app.AddCommand(securityScanMdFromCmd(opts))

	app.PersistentFlags().StringVar(&opts.HugoDataSoloOpts.version, "version", "", "version of docs and code")
	app.PersistentFlags().StringVar(&opts.HugoDataSoloOpts.product, "product", "gloo", "product to which the docs refer (defaults to gloo)")
	app.PersistentFlags().BoolVar(&opts.HugoDataSoloOpts.noScope, "no-scope", false, "if set, will not nest the served docs by product or version")
	app.PersistentFlags().BoolVar(&opts.HugoDataSoloOpts.callLatest, "call-latest", false, "if set, will use the string 'latest' in the scope, rather than the particular release version")

	return app
}

func securityScanMdFromCmd(opts *options) *cobra.Command {
	app := &cobra.Command{
		Use:   "gen-security-scan-md",
		Short: "generate a markdown file from gcloud bucket",
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(skipSecurityScan) != "" {
				return nil
			}
			return generateSecurityScanMd(args)
		},
	}
	return app
}

func changelogMdFromGithubCmd(opts *options) *cobra.Command {
	app := &cobra.Command{
		Use:   "gen-changelog-md",
		Short: "generate a markdown file from Github Release pages API",
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(skipChangelogGeneration) != "" {
				return nil
			}
			return generateChangelogMd(args)
		},
	}
	return app
}

const (
	latestVersionPath = "latest"
)

const (
	glooDocGen              = "gloo"
	glooEDocGen             = "glooe"
	skipChangelogGeneration = "SKIP_CHANGELOG_GENERATION"
	skipSecurityScan        = "SKIP_SECURITY_SCAN"
)

const (
	glooOpenSourceRepo = "gloo"
	glooEnterpriseRepo = "solo-projects"
)

var (
	InvalidInputError = func(arg string) error {
		return eris.Errorf("invalid input, must provide exactly one argument, either '%v' or '%v', (provided %v)",
			glooDocGen,
			glooEDocGen,
			arg)
	}
	MissingGithubTokenError = func(envVar string) error {
		return eris.Errorf("Must either set GITHUB_TOKEN or set %s environment variable to true", envVar)
	}
)

// Default FindDependentVersionFn (used for Gloo Edge)
func FindDependentVersionFn(enterpriseVersion *Version) (*Version, error) {
	versionTag := enterpriseVersion.String()
	dependencyUrl := fmt.Sprintf("https://storage.googleapis.com/gloo-ee-dependencies/%s/dependencies", versionTag[1:])
	request, err := http.NewRequest("GET", dependencyUrl, nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(`.*gloo.*(v.*)`)
	if err != nil {
		return nil, err
	}
	matches := re.FindStringSubmatch(string(body))
	if len(matches) != 2 {
		return nil, eris.Errorf("unable to get gloo dependency for gloo enterprise version %s\n response from google storage API: %s", versionTag, string(body))
	}
	glooVersionTag := matches[1]
	version, err := ParseVersion(glooVersionTag)
	if err != nil {
		return nil, err
	}
	return version, nil
}

// Generates changelog for releases as fetched from Github
// Github defaults to a chronological order
func generateChangelogMd(args []string) error {
	if len(args) != 1 {
		return InvalidInputError(fmt.Sprintf("%v", len(args)-1))
	}
	client := github.NewClient(nil)
	target := args[0]
	switch target {
	case glooDocGen:
		generator := changelogdocutils.NewMinorReleaseGroupedChangelogGenerator(client, "solo-io", glooOpenSourceRepo)
		out, err := generator.GenerateJSON(context.Background())
		if err != nil {
			return err
		}
		fmt.Println(out)
	case glooEDocGen:
		err := generateGlooEChangelog()
		if err != nil {
			return err
		}
	default:
		return InvalidInputError(target)
	}

	return nil
}

// Fetches Gloo Enterprise releases, merges in open source release notes, and orders them by version
func generateGlooEChangelog() error {
	// Initialize Auth
	ctx := context.Background()
	if os.Getenv("GITHUB_TOKEN") == "" {
		return MissingGithubTokenError(skipChangelogGeneration)
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	generator := changelogdocutils.NewMergedReleaseGenerator(client, "solo-io", glooEnterpriseRepo, glooOpenSourceRepo, FindDependentVersionFn)
	out, err := generator.GenerateJSON(context.Background())
	if err != nil {
		return err
	}
	fmt.Println(out)
	return nil
}

// Generates security scan log for releases
func generateSecurityScanMd(args []string) error {
	if len(args) != 1 {
		return InvalidInputError(fmt.Sprintf("%v", len(args)-1))
	}
	target := args[0]
	var (
		err error
	)
	switch target {
	case glooDocGen:
		err = generateSecurityScanGloo(context.Background())
	case glooEDocGen:
		err = generateSecurityScanGlooE(context.Background())
	default:
		return InvalidInputError(target)
	}

	return err
}

func generateSecurityScanGloo(ctx context.Context) error {
	client := github.NewClient(nil)
	allReleases, err := githubutils.GetAllRepoReleases(ctx, client, "solo-io", glooOpenSourceRepo)
	if err != nil {
		return err
	}
	githubutils.SortReleasesBySemver(allReleases)
	if err != nil {
		return err
	}

	var tagNames []string
	for _, release := range allReleases {
		// ignore beta releases when display security scan results
		test, err := semver.NewVersion(release.GetTagName())
		stableOnlyConstraint, _ := semver.NewConstraint(">= 1.4.0")
		if err == nil && stableOnlyConstraint.Check(test) {
			tagNames = append(tagNames, release.GetTagName())
		}
	}

	return BuildSecurityScanReportGloo(tagNames)
}

func generateSecurityScanGlooE(ctx context.Context) error {
	// Initialize Auth
	if os.Getenv("GITHUB_TOKEN") == "" {
		return MissingGithubTokenError(skipSecurityScan)
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)
	allReleases, err := githubutils.GetAllRepoReleases(ctx, client, "solo-io", glooEnterpriseRepo)
	if err != nil {
		return err
	}
	githubutils.SortReleasesBySemver(allReleases)
	if err != nil {
		return err
	}

	var tagNames []string
	for _, release := range allReleases {
		// ignore beta releases when display security scan results
		test, err := semver.NewVersion(release.GetTagName())
		stableOnlyConstraint, _ := semver.NewConstraint(">= 1.4.0")
		if err == nil && stableOnlyConstraint.Check(test) {
			tagNames = append(tagNames, release.GetTagName())
		}
	}

	return BuildSecurityScanReportGlooE(tagNames)
}
