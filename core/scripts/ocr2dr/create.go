package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli"

	clcmd "github.com/smartcontractkit/chainlink/core/cmd"
	helpers "github.com/smartcontractkit/chainlink/core/scripts/common"
)

func createBridge(client *clcmd.Client, app *cli.App) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()

	bridgeFile := filepath.Join(templatesDir, bridgeTemplate)
	fileFs := flag.NewFlagSet("test", flag.ExitOnError)
	fileFs.Parse([]string{bridgeFile})
	ctx := cli.NewContext(app, fileFs, nil)
	err := client.CreateBridge(ctx)
	helpers.PanicErr(err)
}

func createJobSpecs(client *clcmd.Client, app *cli.App, nodes []Node) {
	for _, node := range nodes {
		tomlFileName := fmt.Sprintf("%s.toml", node.Host)
		tomlFile := filepath.Join(artefactsDir, tomlFileName)
		fileFs := flag.NewFlagSet("test", flag.ExitOnError)
		fileFs.String("file", tomlFile, "")
		ctx := cli.NewContext(app, fileFs, nil)
		err := client.CreateJob(ctx)
		helpers.PanicErr(err)
	}
}
