//go:build installer

package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/pflag"

	"code-intelligence.com/cifuzz/internal/cmdutils"
	"code-intelligence.com/cifuzz/internal/installer"
	"code-intelligence.com/cifuzz/pkg/log"
	"code-intelligence.com/cifuzz/util/fileutil"
)

//go:embed build
var buildFiles embed.FS

var notes []string

var shells = []string{"bash", "zsh", "fish"}

func main() {
	flags := pflag.NewFlagSet("cifuzz installer", pflag.ExitOnError)
	helpRequested := flags.BoolP("help", "h", false, "")
	flags.Bool("verbose", false, "Print verbose output")
	ignoreCheck := flags.Bool("ignore-installation-check", false, "Doesn't check if a previous installation already exists")
	cmdutils.ViperMustBindPFlag("verbose", flags.Lookup("verbose"))

	err := flags.Parse(os.Args)
	if err != nil {
		log.Error(errors.WithStack(err))
		os.Exit(1)
	}

	if *helpRequested {
		log.Printf("Usage of cifuzz installer:")
		flags.PrintDefaults()
		os.Exit(0)
	}

	installDir, err := installer.GetInstallDir()
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	if !*ignoreCheck {
		// Check if a cifuzz installation exists in a different location
		err = checkExistingCIFuzz(installDir)
		if err != nil {
			log.Error(err)
			os.Exit(1)
		}
	}

	err = installCIFuzz(installDir)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}
}

func installCIFuzz(installDir string) error {
	// Remove the installation directory if it already exists
	exists, _ := fileutil.Exists(installDir)
	if exists {
		log.Printf("Removing %s", installDir)
		err := os.RemoveAll(installDir)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	// Extract the embedded files that were built by the cifuzz builder
	// into the installation directory
	log.Printf("Installing cifuzz to %s", installDir)
	err := extractEmbeddedFiles(&buildFiles, installDir)
	if err != nil {
		return err
	}

	// Create the command completion scripts (not supported on Windows)
	if runtime.GOOS != "windows" {
		for _, shell := range shells {
			err = createCommandCompletionScript(installDir, shell)
			if err != nil {
				return err
			}
		}
	}

	// Install and register the CMake package - unless the user
	// set CIFUZZ_INSTALLER_NO_CMAKE. One use case for not installing
	// CMake is when cifuzz is installed in a sandbox which doesn't
	// allow access to the CMake packages directory.
	if os.Getenv("CIFUZZ_INSTALLER_NO_CMAKE") == "" {
		if runtime.GOOS != "windows" && os.Getuid() == 0 {
			// On non-Windows systems, CMake doesn't have the concept of a system
			// package registry. Instead, install the package into the well-known
			// prefix /usr/local using the following relative search path:
			// /(lib/|lib|share)/<name>*/(cmake|CMake)/
			// See:
			// https://cmake.org/cmake/help/latest/command/find_package.html#config-mode-search-procedure
			// https://gitlab.kitware.com/cmake/cmake/-/blob/5ed9232d781ccfa3a9fae709e12999c6649aca2f/Modules/Platform/UnixPaths.cmake#L30)
			cmakeSrc := filepath.Join(installDir, "share", "cifuzz")
			cmakeDest := "/usr/local/share/cifuzz"
			log.Printf("Creating symlink %s -> %s", cmakeSrc, cmakeDest)
			// Older versions of the installer copied the directory, so
			// we ensure that no directory exists in the destination to
			// avoid fileutil.ForceSymlink to fail
			err = os.RemoveAll(cmakeDest)
			if err != nil {
				return errors.WithStack(err)
			}
			err = os.MkdirAll(filepath.Dir(cmakeDest), 0755)
			if err != nil {
				return errors.WithStack(err)
			}
			err = fileutil.ForceSymlink(cmakeSrc, cmakeDest)
			if err != nil {
				return err
			}
		} else {
			// The CMake package registry entry has to point directly to the directory
			// containing the CIFuzzConfig.cmake file rather than any valid prefix for
			// the config mode search procedure.
			dirForRegistry := filepath.Join(installDir, "share", "cifuzz", "cmake")
			err = installer.RegisterCMakePackage(dirForRegistry)
			if err != nil {
				return err
			}
		}
	}

	// Install the autocompletion script for the current shell (if the
	// shell is supported)
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "bash":
		err = installBashCompletionScript(installDir)
	case "zsh":
		err = installZshCompletionScript(installDir)
	case "fish":
		err = installFishCompletionScript(installDir)
	default:
		log.Printf("Not installing shell completion script: Unsupported shell: %s", shell)
	}
	if err != nil {
		return err
	}

	// Create a symlink to the cifuzz executable
	var symlinkPath string
	if runtime.GOOS != "windows" {
		if os.Getuid() == 0 {
			symlinkPath = "/usr/local/bin/cifuzz"
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return errors.WithStack(err)
			}
			symlinkPath = filepath.Join(home, ".local", "bin", "cifuzz")
		}
		log.Printf("Creating symlink %s", symlinkPath)
		err = os.MkdirAll(filepath.Dir(symlinkPath), 0755)
		if err != nil {
			return errors.WithStack(err)
		}
		err = fileutil.ForceSymlink(cifuzzPath(installDir), symlinkPath)
		if err != nil {
			return err
		}
	}

	log.Success("Installation successful")

	// Print a newline between the "Installation successful" message
	// and the notes
	log.Print()

	for _, note := range notes {
		log.Note(note)
	}

	// Tell the user how to add cifuzz to the PATH (unless it's already
	// in the PATH)
	cifuzzInPATH, err := cifuzzInPATH(installDir)
	if err != nil {
		return err
	}
	if !cifuzzInPATH {
		if runtime.GOOS == "windows" {
			// TODO: On Windows, users generally don't expect having to fiddle with their PATH. We should update it for
			//       them, but that requires asking for admin access.
			log.Notef(`Please add the following directory to your PATH:
	%s
`, filepath.Join(installDir, "bin"))
		} else {
			shell := filepath.Base(os.Getenv("SHELL"))
			var profileName string
			if shell == "bash" {
				profileName = "~/.bash_profile"
			} else if shell == "zsh" {
				profileName = "~/.zprofile"
			} else {
				profileName = "~/.profile"
			}
			log.Notef(`To add cifuzz to your PATH:

    export PATH="$PATH:%s" >> %s

`, filepath.Dir(symlinkPath), profileName)
		}
	}

	return nil
}

func extractEmbeddedFiles(files *embed.FS, installDir string) error {
	// List of files which have to be made executable
	cifuzzExecutable := filepath.Join("bin", "cifuzz")
	executableFiles := []string{
		cifuzzExecutable,
		filepath.Join("bin", "minijail0"),
		filepath.Join("lib", "process_wrapper"),
	}

	buildFS, err := fs.Sub(files, "build")
	if err != nil {
		return errors.WithStack(err)
	}

	// Extract files from the build directory
	err = fs.WalkDir(buildFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return errors.WithStack(err)
		}

		if !d.IsDir() {
			targetDir := filepath.Dir(filepath.Join(installDir, path))

			err = os.MkdirAll(targetDir, 0755)
			if err != nil {
				return errors.WithStack(err)
			}

			content, err := fs.ReadFile(buildFS, path)
			if err != nil {
				return errors.WithStack(err)
			}

			fileName := filepath.Join(targetDir, d.Name())
			err = os.WriteFile(fileName, content, 0644)
			if err != nil {
				return errors.WithStack(err)
			}

			// Make required files executable
			for _, executableFile := range executableFiles {
				if executableFile == path {
					err = os.Chmod(fileName, 0755)
					if err != nil {
						return errors.WithStack(err)
					}
				}
			}
		}

		return nil
	})
	return err
}

func installBashCompletionScript(installDir string) error {
	// Installing the bash completion script is only supported on Linux
	// and macOS
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil
	}

	completionScript := completionScriptPath(installDir, "bash")

	switch runtime.GOOS {
	case "linux":
		var dir string
		if os.Getuid() == 0 {
			// We run as root, so we put the completion script into the
			// system-wide completions directory
			dir = "/etc/bash_completion.d"
		} else {
			// We run as non-root, so install the script to the user's
			// completions directory
			// See https://github.com/scop/bash-completion/tree/2.9#installation
			if os.Getenv("XDG_DATA_HOME") != "" {
				dir = os.Getenv("XDG_DATA_HOME") + "/bash-completion/completions"
			} else {
				dir = os.Getenv("HOME") + "/.local/share/bash-completion/completions"
			}
		}
		err := os.MkdirAll(dir, 0755)
		if err != nil {
			return errors.WithStack(err)
		}
		symlinkPath := filepath.Join(dir, "cifuzz")
		log.Printf("Creating symlink %s", symlinkPath)
		err = fileutil.ForceSymlink(completionScript, symlinkPath)
		if err != nil {
			return err
		}
	case "darwin":
		// There are no bash completion directories on macOS by default,
		// so we need user action to source our installation directory
		notes = append(notes, fmt.Sprintf(`To enable command completion:

    # enable bash completion (if not already enabled):
    echo "[ -f $(brew --prefix)/etc/bash_completion ] && source $(brew --prefix)/etc/bash_completion" >> ~/.bash_profile
    # enable cifuzz completion:
    echo source '%s' >> ~/.bash_profile

`, completionScript))
	}

	return nil
}

func installZshCompletionScript(installDir string) error {
	// Installing the zsh completion script is only supported on Linux
	// and macOS
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil
	}

	// Check if we can write to the first path in the fpath, in which
	// case we also install the completion script into that directory,
	// not requiring any user action.
	//
	// We try to read $ZDOTDIR/.zshrc or ~/.zshrc here in order to
	// store the completion script in the correct directory.
	// When run as non-root, we try to get a user-writeable directory
	// from $fpath[1] by reading ~/.zshrc.
	// When run as root, it's expected that /root/.zshrc doesn't
	// exist, which leaves $fpath[1] at the default which should be only
	// writeable as root.
	cmd := exec.Command("zsh", "-c", ". ${ZDOTDIR:-${HOME}}/.zshrc 2>/dev/null; echo \"$fpath[1]\"")
	cmd.Stderr = os.Stderr
	log.Debugf("Command: %s", cmd.String())
	out, err := cmd.Output()
	if err != nil {
		return errors.WithStack(err)
	}
	fpath := strings.TrimSpace(string(out))

	// Ensure that the directory exists. Ignore errors here, if this
	// fails, creating the symlink below will also fail and we handle
	// the error there
	_ = os.MkdirAll(fpath, 0755)

	// Try to create a symlink in the first fpath directory
	completionScript := completionScriptPath(installDir, "zsh")
	symlinkPath := filepath.Join(fpath, "_cifuzz")
	err = fileutil.ForceSymlink(completionScript, symlinkPath)
	if err != nil {
		// Creating a symlink in the first fpath directory failed, so we
		// tell the user to add the completion script from our install
		// directory to their fpath instead
		notes = append(notes, fmt.Sprintf(`To enable command completion:

    echo fpath=(%s $fpath) >> ~/.zshrc
    echo "autoload -U compinit; compinit" >> ~/.zshrc

`, filepath.Dir(completionScript)))
	} else {
		log.Printf("Creating symlink %s", symlinkPath)
		notes = append(notes, `To enable command completion (if not already enabled):

    echo "autoload -U compinit; compinit" >> ~/.zshrc

`)
	}

	return nil
}

func installFishCompletionScript(installDir string) error {
	// Installing the zsh completion script is only supported on Linux
	// and macOS
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil
	}

	var dir string
	// Choose the correct directory for the completion script.
	// See https://fishshell.com/docs/current/completions.html#where-to-put-completions
	if os.Getuid() == 0 {
		// We run as root, so we put the completion script into the
		// system-wide completions directory
		dir = "/usr/share/fish/vendor_completions.d"
	} else {
		// We run as non-root, so install the script to the user's
		// completions directory
		if os.Getenv("XDG_DATA_HOME") != "" {
			dir = os.Getenv("XDG_DATA_HOME") + "/fish/vendor_completions.d"
		} else {
			dir = os.Getenv("HOME") + "/.local/share/fish/vendor_completions.d"
		}
	}
	err := os.MkdirAll(dir, 0755)
	if err != nil {
		return errors.WithStack(err)
	}

	completionScript := completionScriptPath(installDir, "fish")
	symlinkPath := filepath.Join(dir, "cifuzz.fish")
	log.Printf("Creating symlink %s", symlinkPath)
	err = fileutil.ForceSymlink(completionScript, symlinkPath)
	return errors.WithStack(err)
}

func cifuzzInPATH(installDir string) (bool, error) {
	cifuzzPath, err := exec.LookPath("cifuzz")
	if err != nil {
		return false, nil
	}

	cifuzzPath, err = filepath.EvalSymlinks(cifuzzPath)
	if err != nil {
		return false, errors.WithStack(err)
	}

	return cifuzzPath == filepath.Join(installDir, "bin", "cifuzz"), nil
}

func checkExistingCIFuzz(installDir string) error {
	oldCIFuzzPath, err := exec.LookPath("cifuzz")
	if err != nil {
		// cifuzz was not found
		return nil
	}

	oldCIFuzzPath, err = filepath.EvalSymlinks(oldCIFuzzPath)
	if err != nil {
		return errors.WithStack(err)
	}

	if oldCIFuzzPath == filepath.Join(installDir, "bin", "cifuzz") {
		// The old cifuzz will be replaced because it's installed in the
		// same directory as we are about to install to
		return nil
	}

	return errors.Errorf(`cifuzz is already installed in %s.
To avoid issues with incompatible versions, please uninstall cifuzz first.
See https://github.com/CodeIntelligenceTesting/cifuzz#uninstall`, oldCIFuzzPath)
}

func createCommandCompletionScript(installDir, shell string) error {
	cifuzz := cifuzzPath(installDir)
	completionScript := completionScriptPath(installDir, shell)
	err := os.MkdirAll(filepath.Dir(completionScript), 0700)
	if err != nil {
		return errors.WithStack(err)
	}

	cmd := exec.Command("sh", "-c", "'"+cifuzz+"' completion "+shell+" > '"+completionScript+"'")
	cmd.Stderr = os.Stderr
	log.Debugf("Command: %s", cmd.String())
	err = cmd.Run()
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func completionScriptPath(installDir, shell string) string {
	return filepath.Join(installDir, "share", "cifuzz", "completions", shell)
}

func cifuzzPath(installDir string) string {
	return filepath.Join(installDir, "bin", "cifuzz")
}
