package cmd

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/haveachin/infrared/internal/app/infrared"
	"github.com/haveachin/infrared/internal/pkg/bedrock"
	"github.com/haveachin/infrared/internal/pkg/config"
	"github.com/haveachin/infrared/internal/pkg/java"
	"github.com/haveachin/infrared/internal/plugin/api"
	"github.com/haveachin/infrared/internal/plugin/prometheus"
	"github.com/haveachin/infrared/internal/plugin/traffic_limiter"
	"github.com/haveachin/infrared/internal/plugin/webhook"
	"github.com/haveachin/infrared/pkg/event"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

const (
	devEnvironment  = "dev"
	prodEnvironment = "prod"

	configDefaultPath = "configs/"
)

var (
	files   embed.FS
	version string

	configPath  string
	workingDir  string
	environment string

	logger        *zap.Logger
	pluginManager infrared.PluginManager

	mu      sync.Mutex
	proxies = map[infrared.Edition]infrared.Proxy{}

	rootCmd = &cobra.Command{
		Use:   "infrared",
		Short: "Starts the infrared proxy",
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			logger, err = newLogger(environment)
			if err != nil {
				return err
			}
			defer logger.Sync()

			if err := os.Chdir(workingDir); err != nil {
				return err
			}

			logger.Info("loading proxy from config",
				zap.String("config", configPath),
			)

			if _, err := os.Stat(configPath); err != nil && errors.Is(err, os.ErrNotExist) {
				if err := safeWriteFromEmbeddedFS(configDefaultPath, "."); err != nil {
					return err
				}
			}

			cfg, err := config.New(configPath, onConfigChange, logger)
			if err != nil {
				return err
			}

			data, err := cfg.Read()
			if err != nil {
				return err
			}

			javaPrxCfg, err := java.NewProxyConfigFromMap(data)
			if err != nil {
				return err
			}

			javaPrx, err := infrared.NewProxy(javaPrxCfg)
			if err != nil {
				return err
			}
			proxies[infrared.JavaEdition] = javaPrx

			bedrockPrxCfg, err := bedrock.NewProxyConfigFromMap(data)
			if err != nil {
				return err
			}

			bedrockPrx, err := infrared.NewProxy(bedrockPrxCfg)
			if err != nil {
				return err
			}
			proxies[infrared.BedrockEdition] = bedrockPrx

			eventBus := event.NewInternalBus()
			pluginManager = infrared.PluginManager{
				Proxies: proxies,
				Plugins: []infrared.Plugin{
					&webhook.Plugin{},
					&prometheus.Plugin{},
					&api.Plugin{},
					&traffic_limiter.Plugin{},
				},
				Logger:   logger,
				EventBus: eventBus,
			}
			logger.Info("loading plugins")
			pluginManager.LoadPlugins(data)
			logger.Info("enabling plugins")
			pluginManager.EnablePlugins()
			defer pluginManager.DisablePlugins()

			logger.Info("starting proxies")
			for _, proxy := range proxies {
				go proxy.ListenAndServe(eventBus, logger)
			}

			sc := make(chan os.Signal, 1)
			signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
			<-sc
			return nil
		},
	}
)

func init() {
	rootCmd.PersistentFlags().StringVarP(&workingDir, "working-dir", "w", ".", "set the working directory")
	rootCmd.PersistentFlags().StringVarP(&environment, "environment", "e", "prod", "set the deployment environment")
	rootCmd.Flags().StringVarP(&configPath, "config", "c", "config.yml", "path of the config file")

	rootCmd.AddCommand(licenseCmd)
	rootCmd.AddCommand(versionCmd)
	// Migration is not implemented yet
	// rootCmd.AddCommand(migrateCmd)
}

func newLogger(env string) (*zap.Logger, error) {
	switch env {
	case devEnvironment:
		return zap.NewDevelopment()
	case prodEnvironment:
		return zap.NewProduction()
	default:
		return nil, fmt.Errorf("unsupported environment %q", environment)
	}
}

// Execute executes the root command.
func Execute(fs embed.FS, v string) error {
	files = fs
	version = v
	return rootCmd.Execute()
}

func safeWriteFromEmbeddedFS(embedPath, sysPath string) error {
	entries, err := files.ReadDir(embedPath)
	if err != nil {
		return err
	}

	for _, e := range entries {
		ePath := fmt.Sprintf("%s/%s", embedPath, e.Name())
		sPath := filepath.Join(sysPath, e.Name())

		if _, err := os.Stat(sPath); err == nil || !os.IsNotExist(err) {
			continue
		}

		if e.IsDir() {
			if err := os.Mkdir(sPath, 0755); err != nil {
				return err
			}

			safeWriteFromEmbeddedFS(ePath, sPath)
			continue
		}

		bb, err := files.ReadFile(ePath)
		if err != nil {
			return err
		}

		if err := os.WriteFile(sPath, bb, 0755); err != nil {
			return err
		}
	}

	return nil
}

func onConfigChange(cfg map[string]any) {
	mu.Lock()
	defer mu.Unlock()

	javaPrxCfg, err := java.NewProxyConfigFromMap(cfg)
	if err != nil {
		logger.Error("failed to load java config",
			zap.Error(err),
		)
		return
	}

	bedrockPrxCfg, err := bedrock.NewProxyConfigFromMap(cfg)
	if err != nil {
		logger.Error("failed to load bedrock config",
			zap.Error(err),
		)
		return
	}

	prxCfgs := map[infrared.Edition]infrared.ProxyConfig{
		infrared.JavaEdition:    javaPrxCfg,
		infrared.BedrockEdition: bedrockPrxCfg,
	}

	logger.Debug("Reloading proxies")
	for n, p := range proxies {
		if err := p.Reload(prxCfgs[n]); err != nil {
			logger.Error("failed to reload proxy",
				zap.Error(err),
			)
		}
	}

	logger.Debug("Reloading plugins")
	pluginManager.ReloadPlugins(cfg)
}
