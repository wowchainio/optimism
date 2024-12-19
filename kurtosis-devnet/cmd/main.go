package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/ethereum-optimism/optimism/kurtosis-devnet/pkg/build"
	"github.com/ethereum-optimism/optimism/kurtosis-devnet/pkg/kurtosis"
	"github.com/ethereum-optimism/optimism/kurtosis-devnet/pkg/serve"
	"github.com/ethereum-optimism/optimism/kurtosis-devnet/pkg/tmpl"
)

type config struct {
	templateFile    string
	dataFile        string
	kurtosisPackage string
	enclave         string
	environment     string
	dryRun          bool
	localHostName   string
	baseDir         string
}

func parseFlags(fs *flag.FlagSet, args []string) (*config, error) {
	cfg := &config{}

	fs.StringVar(&cfg.templateFile, "template", "", "Path to the template file (required)")
	fs.StringVar(&cfg.dataFile, "data", "", "Path to JSON data file (optional)")
	fs.StringVar(&cfg.kurtosisPackage, "kurtosis-package", kurtosis.DefaultPackageName, "Kurtosis package to deploy (optional)")
	fs.StringVar(&cfg.enclave, "enclave", kurtosis.DefaultEnclave, "Enclave name (optional)")
	fs.StringVar(&cfg.environment, "environment", "", "Path to JSON environment file output (optional)")
	fs.BoolVar(&cfg.dryRun, "dry-run", false, "Dry run mode (optional)")
	fs.StringVar(&cfg.localHostName, "local-hostname", "host.docker.internal", "DNS for localhost from Kurtosis perspective (optional)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Validate required flags
	if cfg.templateFile == "" {
		return nil, fmt.Errorf("template file is required")
	}
	cfg.baseDir = filepath.Dir(cfg.templateFile)

	return cfg, nil
}

type staticServer struct {
	dir string
	*serve.Server
}

func launchStaticServer(ctx context.Context, cfg *config) (*staticServer, func(), error) {
	// we will serve content from this tmpDir for the duration of the devnet creation
	tmpDir, err := os.MkdirTemp("", "contracts-bundle")
	if err != nil {
		return nil, nil, fmt.Errorf("error creating temporary directory: %w", err)
	}

	server := serve.NewServer(
		serve.WithStaticDir(tmpDir),
		serve.WithHostname(cfg.localHostName),
	)
	if err := server.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("error starting server: %w", err)
	}

	return &staticServer{
			dir:    tmpDir,
			Server: server,
		}, func() {
			if err := server.Stop(ctx); err != nil {
				log.Printf("Error stopping server: %v\n", err)
			}
			if err := os.RemoveAll(tmpDir); err != nil {
				log.Printf("Error removing temporary directory: %v\n", err)
			}
		}, nil
}

func localDockerImageOption(cfg *config) tmpl.TemplateContextOptions {
	dockerBuilder := build.NewDockerBuilder(
		build.WithDockerBaseDir(cfg.baseDir),
		build.WithDockerDryRun(cfg.dryRun),
	)

	imageTag := func(projectName string) string {
		return fmt.Sprintf("%s:%s", projectName, cfg.enclave)
	}

	return tmpl.WithFunction("localDockerImage", func(projectName string) (string, error) {
		return dockerBuilder.Build(projectName, imageTag(projectName))
	})
}

func localContractArtifactsOption(cfg *config, server *staticServer) tmpl.TemplateContextOptions {
	contractsBundle := fmt.Sprintf("contracts-bundle-%s.tar.gz", cfg.enclave)
	contractsBundlePath := func(_ string) string {
		return filepath.Join(server.dir, contractsBundle)
	}

	contractBuilder := build.NewContractBuilder(
		build.WithContractBaseDir(cfg.baseDir),
		build.WithContractDryRun(cfg.dryRun),
	)

	return tmpl.WithFunction("localContractArtifacts", func(layer string) (string, error) {
		bundlePath := contractsBundlePath(layer)
		// we're in a temp dir, so we can skip the build if the file already
		// exists: it'll be the same file! In particular, since we're ignoring
		// layer for now, skip the 2nd build.
		if _, err := os.Stat(bundlePath); err != nil {
			if err := contractBuilder.Build(layer, bundlePath); err != nil {
				return "", err
			}
		}

		url := fmt.Sprintf("%s/%s", server.URL(), contractsBundle)
		log.Printf("%s: contract artifacts available at: %s\n", layer, url)
		return url, nil
	})
}

func renderTemplate(cfg *config, server *staticServer) (*bytes.Buffer, error) {
	opts := []tmpl.TemplateContextOptions{
		localDockerImageOption(cfg),
		localContractArtifactsOption(cfg, server),
	}

	// Read and parse the data file if provided
	if cfg.dataFile != "" {
		data, err := os.ReadFile(cfg.dataFile)
		if err != nil {
			return nil, fmt.Errorf("error reading data file: %w", err)
		}

		var templateData map[string]interface{}
		if err := json.Unmarshal(data, &templateData); err != nil {
			return nil, fmt.Errorf("error parsing JSON data: %w", err)
		}

		opts = append(opts, tmpl.WithData(templateData))
	}

	// Open template file
	tmplFile, err := os.Open(cfg.templateFile)
	if err != nil {
		return nil, fmt.Errorf("error opening template file: %w", err)
	}
	defer tmplFile.Close()

	// Create template context
	tmplCtx := tmpl.NewTemplateContext(opts...)

	// Process template
	buf := bytes.NewBuffer(nil)
	if err := tmplCtx.InstantiateTemplate(tmplFile, buf); err != nil {
		return nil, fmt.Errorf("error processing template: %w", err)
	}

	return buf, nil
}

func deploy(ctx context.Context, cfg *config, r io.Reader) error {
	// Create a multi reader to output deployment input to stdout
	buf := bytes.NewBuffer(nil)
	tee := io.TeeReader(r, buf)

	// Log the deployment input
	log.Println("Deployment input:")
	if _, err := io.Copy(os.Stdout, tee); err != nil {
		return fmt.Errorf("error copying deployment input: %w", err)
	}

	kurtosisDeployer := kurtosis.NewKurtosisDeployer(
		kurtosis.WithKurtosisBaseDir(cfg.baseDir),
		kurtosis.WithKurtosisDryRun(cfg.dryRun),
		kurtosis.WithKurtosisPackageName(cfg.kurtosisPackage),
		kurtosis.WithKurtosisEnclave(cfg.enclave),
	)

	env, err := kurtosisDeployer.Deploy(ctx, buf)
	if err != nil {
		return fmt.Errorf("error deploying kurtosis: %w", err)
	}

	envOutput := os.Stdout
	if cfg.environment != "" {
		envOutput, err = os.Create(cfg.environment)
		if err != nil {
			return fmt.Errorf("error creating environment file: %w", err)
		}
		defer envOutput.Close()
	} else {
		log.Println("Environment description:")
	}

	enc := json.NewEncoder(envOutput)
	enc.SetIndent("", "  ")
	if err := enc.Encode(env); err != nil {
		return fmt.Errorf("error encoding environment: %w", err)
	}

	return nil
}

func mainFunc(cfg *config) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server, cleanup, err := launchStaticServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("error launching static server: %w", err)
	}
	defer cleanup()

	buf, err := renderTemplate(cfg, server)
	if err != nil {
		return fmt.Errorf("error rendering template: %w", err)
	}

	return deploy(ctx, cfg, buf)
}

func main() {
	cfg, err := parseFlags(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:])
	if err != nil {
		flag.Usage()
		log.Fatalf("Error parsing flags: %v\n", err)
	}

	if err := mainFunc(cfg); err != nil {
		log.Fatalf("Error: %v\n", err)
	}
}
