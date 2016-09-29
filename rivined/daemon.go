package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rivine/rivine/api"
	"github.com/rivine/rivine/build"
	"github.com/rivine/rivine/modules"
	"github.com/rivine/rivine/modules/consensus"
	"github.com/rivine/rivine/modules/explorer"
	"github.com/rivine/rivine/modules/gateway"
	"github.com/rivine/rivine/modules/transactionpool"
	"github.com/rivine/rivine/modules/wallet"
	"github.com/rivine/rivine/profile"

	"github.com/bgentry/speakeasy"
	"github.com/spf13/cobra"
)

// verifyAPISecurity checks that the security values are consistent with a
// sane, secure system.
func verifyAPISecurity(config Config) error {
	// Make sure that only the loopback address is allowed unless the
	// --disable-api-security flag has been used.
	if !config.Siad.AllowAPIBind {
		addr := modules.NetAddress(config.Siad.APIaddr)
		if !addr.IsLoopback() {
			if addr.Host() == "" {
				return fmt.Errorf("a blank host will listen on all interfaces, did you mean localhost:%v?\nyou must pass --disable-api-security to bind Siad to a non-localhost address", addr.Port())
			}
			return errors.New("you must pass --disable-api-security to bind Siad to a non-localhost address")
		}
		return nil
	}

	// If the --disable-api-security flag is used, enforce that
	// --authenticate-api must also be used.
	if config.Siad.AllowAPIBind && !config.Siad.AuthenticateAPI {
		return errors.New("cannot use --disable-api-security without setting an api password")
	}
	return nil
}

// processNetAddr adds a ':' to a bare integer, so that it is a proper port
// number.
func processNetAddr(addr string) string {
	_, err := strconv.Atoi(addr)
	if err == nil {
		return ":" + addr
	}
	return addr
}

// processModules makes the modules string lowercase to make checking if a
// module in the string easier, and returns an error if the string contains an
// invalid module character.
func processModules(modules string) (string, error) {
	modules = strings.ToLower(modules)
	validModules := "cgtwe"
	invalidModules := modules
	for _, m := range validModules {
		invalidModules = strings.Replace(invalidModules, string(m), "", 1)
	}
	if len(invalidModules) > 0 {
		return "", errors.New("Unable to parse --modules flag, unrecognized or duplicate modules: " + invalidModules)
	}
	return modules, nil
}

// processConfig checks the configuration values and performs cleanup on
// incorrect-but-allowed values.
func processConfig(config Config) (Config, error) {
	var err1 error
	config.Siad.APIaddr = processNetAddr(config.Siad.APIaddr)
	config.Siad.RPCaddr = processNetAddr(config.Siad.RPCaddr)
	config.Siad.HostAddr = processNetAddr(config.Siad.HostAddr)
	config.Siad.Modules, err1 = processModules(config.Siad.Modules)
	err2 := verifyAPISecurity(config)
	err := build.JoinErrors([]error{err1, err2}, ", and ")
	if err != nil {
		return Config{}, err
	}
	return config, nil
}

// startDaemon uses the config parameters to initialize Sia modules and start
// siad.
func startDaemon(config Config) (err error) {
	// Prompt user for API password.
	if config.Siad.AuthenticateAPI {
		config.APIPassword, err = speakeasy.Ask("Enter API password: ")
		if err != nil {
			return err
		}
		if config.APIPassword == "" {
			return errors.New("password cannot be blank")
		}
	}

	// Process the config variables after they are parsed by cobra.
	config, err = processConfig(config)
	if err != nil {
		return err
	}

	// Print a startup message.
	fmt.Println("Loading...")
	loadStart := time.Now()

	// Create the server and start serving daemon routes immediately.
	fmt.Printf("(0/%d) Loading rivined...\n", len(config.Siad.Modules))
	srv, err := NewServer(config.Siad.APIaddr, config.Siad.RequiredUserAgent, config.APIPassword)
	if err != nil {
		return err
	}

	servErrs := make(chan error)
	go func() {
		servErrs <- srv.Serve()
	}()

	// Initialize the Sia modules
	i := 0
	var g modules.Gateway
	if strings.Contains(config.Siad.Modules, "g") {
		i++
		fmt.Printf("(%d/%d) Loading gateway...\n", i, len(config.Siad.Modules))
		g, err = gateway.New(config.Siad.RPCaddr, !config.Siad.NoBootstrap, filepath.Join(config.Siad.SiaDir, modules.GatewayDir))
		if err != nil {
			return err
		}
		defer func() {
			fmt.Println("Closing gateway...")
			err := g.Close()
			if err != nil {
				fmt.Println("Error during gateway shutdown:", err)
			}
		}()

	}
	var cs modules.ConsensusSet
	if strings.Contains(config.Siad.Modules, "c") {
		i++
		fmt.Printf("(%d/%d) Loading consensus...\n", i, len(config.Siad.Modules))
		cs, err = consensus.New(g, !config.Siad.NoBootstrap, filepath.Join(config.Siad.SiaDir, modules.ConsensusDir))
		if err != nil {
			return err
		}
		defer func() {
			fmt.Println("Closing consensus set...")
			err := cs.Close()
			if err != nil {
				fmt.Println("Error during consensus set shutdown:", err)
			}
		}()

	}
	var e modules.Explorer
	if strings.Contains(config.Siad.Modules, "e") {
		i++
		fmt.Printf("(%d/%d) Loading explorer...\n", i, len(config.Siad.Modules))
		e, err = explorer.New(cs, filepath.Join(config.Siad.SiaDir, modules.ExplorerDir))
		if err != nil {
			return err
		}
		defer func() {
			fmt.Println("Closing explorer...")
			err := e.Close()
			if err != nil {
				fmt.Println("Error during explorer shutdown:", err)
			}
		}()

	}
	var tpool modules.TransactionPool
	if strings.Contains(config.Siad.Modules, "t") {
		i++
		fmt.Printf("(%d/%d) Loading transaction pool...\n", i, len(config.Siad.Modules))
		tpool, err = transactionpool.New(cs, g, filepath.Join(config.Siad.SiaDir, modules.TransactionPoolDir))
		if err != nil {
			return err
		}
		defer func() {
			fmt.Println("Closing transaction pool...")
			err := tpool.Close()
			if err != nil {
				fmt.Println("Error during transaction pool shutdown:", err)
			}
		}()

	}
	var w modules.Wallet
	if strings.Contains(config.Siad.Modules, "w") {
		i++
		fmt.Printf("(%d/%d) Loading wallet...\n", i, len(config.Siad.Modules))
		w, err = wallet.New(cs, tpool, filepath.Join(config.Siad.SiaDir, modules.WalletDir))
		if err != nil {
			return err
		}
		defer func() {
			fmt.Println("Closing wallet...")
			err := w.Close()
			if err != nil {
				fmt.Println("Error during wallet shutdown:", err)
			}
		}()

	}

	// Create the Sia API
	a := api.New(
		config.Siad.RequiredUserAgent,
		config.APIPassword,
		cs,
		e,
		g,
		tpool,
		w,
	)

	// connect the API to the server
	srv.mux.Handle("/", a)

	// stop the server if a kill signal is caught
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, os.Kill)
	go func() {
		<-sigChan
		fmt.Println("\rCaught stop signal, quitting...")
		srv.Close()
	}()

	// Print a 'startup complete' message.
	startupTime := time.Since(loadStart)
	fmt.Println("Finished loading in", startupTime.Seconds(), "seconds")

	err = <-servErrs
	if err != nil {
		build.Critical(err)
	}

	return nil
}

// startDaemonCmd is a passthrough function for startDaemon.
func startDaemonCmd(cmd *cobra.Command, _ []string) {
	// Create the profiling directory if profiling is enabled.
	if globalConfig.Siad.Profile || build.DEBUG {
		go profile.StartContinuousProfile(globalConfig.Siad.ProfileDir)
	}

	// Start siad. startDaemon will only return when it is shutting down.
	err := startDaemon(globalConfig)
	if err != nil {
		die(err)
	}
}
