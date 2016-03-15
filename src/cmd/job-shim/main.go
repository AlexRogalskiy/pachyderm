package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/pfs/fuse"
	ppsclient "github.com/pachyderm/pachyderm/src/client/pps"
	"github.com/spf13/cobra"
	"go.pedge.io/env"
	"go.pedge.io/lion"
	"go.pedge.io/pkg/exec"
	"golang.org/x/net/context"
)

type appEnv struct {
	PachydermAddress string `env:"PACHD_PORT_650_TCP_ADDR,required"`
}

func main() {
	env.Main(do, &appEnv{})
}

func do(appEnvObj interface{}) error {
	lion.SetLevel(lion.LevelDebug)
	appEnv := appEnvObj.(*appEnv)
	rootCmd := &cobra.Command{
		Use:   os.Args[0] + " job-id",
		Short: `Pachyderm job-shim, coordinates with ppsd to create an output commit and run user work.`,
		Long:  `Pachyderm job-shim, coordinates with ppsd to create an output commit and run user work.`,
		Run: func(cmd *cobra.Command, args []string) {
			client, err := client.NewFromAddress(fmt.Sprintf("%v:650",appEnv.PachydermAddress))
			if err != nil {
				errorAndExit(err.Error())
			}
			response, err := client.StartJob(
				context.Background(),
				&ppsclient.StartJobRequest{
					Job: &ppsclient.Job{
						ID: args[0],
					}})
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s\n", err.Error())
				os.Exit(0)
			}

			mounter := fuse.NewMounter(appEnv.PachydermAddress, client)
			ready := make(chan bool)
			go func() {
				if err := mounter.Mount(
					"/pfs",
					nil,
					response.CommitMounts,
					ready,
				); err != nil {
					errorAndExit(err.Error())
				}
			}()
			<-ready
			defer func() {
				if err := mounter.Unmount("/pfs"); err != nil {
					errorAndExit(err.Error())
				}
			}()
			var readers []io.Reader
			for _, line := range response.Transform.Stdin {
				readers = append(readers, strings.NewReader(line+"\n"))
			}
			io := pkgexec.IO{
				Stdin:  io.MultiReader(readers...),
				Stdout: os.Stdout,
				Stderr: os.Stderr,
			}
			success := true
			if err := pkgexec.RunIO(io, response.Transform.Cmd...); err != nil {
				fmt.Fprintf(os.Stderr, "%s\n", err.Error())
				success = false
			}
			if _, err := client.FinishJob(
				context.Background(),
				&ppsclient.FinishJobRequest{
					Job: &ppsclient.Job{
						ID: args[0],
					},
					Index:   response.Index,
					Success: success,
				},
			); err != nil {
				errorAndExit(err.Error())
			}
		},
	}

	return rootCmd.Execute()
}

func errorAndExit(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "%s\n", fmt.Sprintf(format, args...))
	os.Exit(1)
}
