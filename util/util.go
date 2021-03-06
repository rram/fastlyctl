package util

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"

	"github.com/alienth/go-fastly"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/urfave/cli"
)

var ErrNonInteractive = errors.New("In non-interactive shell and --assume-yes not used.")

func GetServiceByName(client *fastly.Client, name string) (*fastly.Service, error) {
	var service *fastly.Service
	service, _, err := client.Service.Search(name)
	if err != nil {
		return nil, err
	}
	return service, nil
}

func GetDictionaryByName(client *fastly.Client, serviceName, dictName string) (*fastly.Dictionary, error) {
	var err error
	service, err := GetServiceByName(client, serviceName)
	if err != nil {
		return nil, err
	}
	activeVersion, err := GetActiveVersion(service)
	if err != nil {
		return nil, err
	}
	_ = activeVersion

	dictionary, _, err := client.Dictionary.Get(service.ID, activeVersion, dictName)
	if err != nil {
		return nil, err
	}

	return dictionary, err
}

// getActiveVersion takes in a *fastly.Service and spits out the config version
// that is currently active for that service.
func GetActiveVersion(service *fastly.Service) (uint, error) {
	// Depending on how the service was fetched, it may or may not
	// have a filled Version field. For example, services fetched with a
	// List call do not have their active version in Version.
	if service.Version != 0 {
		return service.Version, nil
	} else {
		for _, version := range service.Versions {
			if version.Active {
				return version.Number, nil
			}
		}
	}
	return 0, fmt.Errorf("Unable to find the active version for service %s", service.Name)
}

func Prompt(question string) (bool, error) {
	var input string
	for {
		fmt.Printf("%s (y/n): ", question)
		if _, err := fmt.Scanln(&input); err != nil {
			return false, err
		}
		if input == "y" {
			return true, nil
		} else if input == "n" {
			return false, nil
		} else {
			fmt.Printf("Invalid input: %s", input)
		}
	}
}

func CountChanges(diff *string) (int, int) {
	removals := regexp.MustCompile(`(^|\n)\-`)
	additions := regexp.MustCompile(`(^|\n)\+`)
	return len(additions.FindAllString(*diff, -1)), len(removals.FindAllString(*diff, -1))
}

func ActivateVersion(c *cli.Context, client *fastly.Client, s *fastly.Service, v *fastly.Version) error {
	activeVersion, err := GetActiveVersion(s)
	if err != nil {
		return err
	}
	assumeYes := c.GlobalBool("assume-yes")
	diff, err := GetUnifiedDiff(client, s, activeVersion, v.Number)
	if err != nil {
		return err
	}

	interactive := IsInteractive()
	if !interactive && !assumeYes {
		return cli.NewExitError(ErrNonInteractive.Error(), -1)
	}
	pager := GetPager()

	fmt.Printf("Diff URL: %s\n", GetDiffUrl(s, activeVersion, v.Number).String())

	additions, removals := CountChanges(&diff)
	var proceed bool
	if !assumeYes {
		if proceed, err = Prompt(fmt.Sprintf("%d additions and %d removals in diff. View?", additions, removals)); err != nil {
			return err
		}
	}

	if proceed || assumeYes {
		if pager != nil && interactive && !assumeYes {
			r, stdin := io.Pipe()
			pager.Stdin = r
			pager.Stdout = os.Stdout
			pager.Stderr = os.Stderr

			c := make(chan struct{})
			go func() {
				defer close(c)
				pager.Run()
			}()

			fmt.Fprintf(stdin, diff)
			stdin.Close()
			<-c
		} else {
			fmt.Printf("Diff for %s:\n\n", s.Name)
			fmt.Println(diff)
		}
	}

	if !c.Bool("noop") {
		if !assumeYes {
			if proceed, err = Prompt("Activate version " + strconv.Itoa(int(v.Number)) + " for service " + s.Name + "?"); err != nil {
				return err
			}
		}
		if proceed || assumeYes {
			if _, _, err = client.Version.Activate(s.ID, v.Number); err != nil {
				return err
			}
			fmt.Printf("Activated version %d for %s. Old version: %d\n", v.Number, s.Name, activeVersion)
		}
	}
	return nil
}

// validateVersion takes in a service and version number and returns an
// error if the version is invalid.
func ValidateVersion(client *fastly.Client, service *fastly.Service, version uint) error {
	validationResponse, _, err := client.Version.Validate(service.ID, version)
	if err != nil {
		return fmt.Errorf("Error validating version: %s", err)
	}

	prefix := fmt.Sprintf("Version %d on service %s", version, service.Name)
	if validationResponse.Status == "error" {
		return fmt.Errorf("%s failed to validate:\n%s\n", prefix, validationResponse.Message)
	} else if len(validationResponse.Warnings) > 0 {
		fmt.Printf("%s validated with warniings:\n%s\n", prefix, validationResponse.Message)
		return nil
	} else if validationResponse.Status == "ok" {
		fmt.Printf("%s successfully validated!\n", prefix)
		return nil
	}

	return fmt.Errorf("Unexpected validation response: %+v", validationResponse)
}

// Returns true if two versions of a given service are identical.  Generated
// VCL is not suitable as the ordering output of GeneratedVCL will vary if a
// no-op change has been made to a config (for example, removing and re-adding
// all domains). As such, this function generates a known-noop diff by
// comparing a version with itself, and then generating a diff between the from
// and to versions.  If the two diffs are identical, then there is no
// difference between from and to.
func VersionsEqual(c *fastly.Client, s *fastly.Service, from, to uint) (bool, error) {
	noDiff, _, err := c.Diff.Get(s.ID, from, from, "text")
	if err != nil {
		return false, err
	}
	diff, _, err := c.Diff.Get(s.ID, from, to, "text")
	if err != nil {
		return false, err
	}
	return noDiff.Diff == diff.Diff, nil
}

func GetUnifiedDiff(c *fastly.Client, s *fastly.Service, from, to uint) (string, error) {
	var fromConfig, toConfig *fastly.Diff
	var err error
	if fromConfig, _, err = c.Diff.Get(s.ID, from, from, "text"); err != nil {
		return "", err
	}
	if toConfig, _, err = c.Diff.Get(s.ID, to, to, "text"); err != nil {
		return "", err
	}

	diff := difflib.UnifiedDiff{
		A:       difflib.SplitLines(fromConfig.Diff),
		B:       difflib.SplitLines(toConfig.Diff),
		Context: 3,
	}
	unified, err := difflib.GetUnifiedDiffString(diff)
	if err != nil {
		return unified, err
	}

	return unified, nil
}

func StringInSlice(check string, slice []string) bool {
	for _, element := range slice {
		if element == check {
			return true
		}
	}
	return false
}

func GetPager() *exec.Cmd {
	for _, pager := range [3]string{os.Getenv("PAGER"), "pager", "less"} {
		// we expect some NotFounds, so ignore errors
		path, _ := exec.LookPath(pager)
		if path != "" {
			return exec.Command(path)
		}
	}
	return nil
}

func CheckFastlyKey(c *cli.Context) *cli.ExitError {
	if c.GlobalString("fastly-key") == "" {
		return cli.NewExitError("Error: Fastly API key must be set.", -1)
	}
	return nil
}

func GetFastlyKey() string {
	file := "fastly_key"
	if _, err := os.Stat(file); err == nil {
		contents, _ := ioutil.ReadFile(file)
		if contents[len(contents)-1] == []byte("\n")[0] {
			contents = contents[:len(contents)-1]
		}
		return string(contents)
	}
	return ""
}

func GetDiffUrl(s *fastly.Service, from, to uint) *url.URL {
	u, _ := url.Parse(fmt.Sprintf("https://manage.fastly.com/configure/services/%s/diff/%d,%d", s.ID, from, to))
	return u
}
