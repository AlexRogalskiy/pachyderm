package main

import (
	gotls "crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path"
	"runtime/debug"
	"runtime/pprof"

	adminclient "github.com/pachyderm/pachyderm/v2/src/admin"
	authclient "github.com/pachyderm/pachyderm/v2/src/auth"
	"github.com/pachyderm/pachyderm/v2/src/client"
	debugclient "github.com/pachyderm/pachyderm/v2/src/debug"
	eprsclient "github.com/pachyderm/pachyderm/v2/src/enterprise"
	identityclient "github.com/pachyderm/pachyderm/v2/src/identity"
	"github.com/pachyderm/pachyderm/v2/src/internal/clusterstate"
	"github.com/pachyderm/pachyderm/v2/src/internal/cmdutil"
	col "github.com/pachyderm/pachyderm/v2/src/internal/collection"
	"github.com/pachyderm/pachyderm/v2/src/internal/dbutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/errors"
	"github.com/pachyderm/pachyderm/v2/src/internal/grpcutil"
	logutil "github.com/pachyderm/pachyderm/v2/src/internal/log"
	"github.com/pachyderm/pachyderm/v2/src/internal/metrics"
	authmw "github.com/pachyderm/pachyderm/v2/src/internal/middleware/auth"
	version_middleware "github.com/pachyderm/pachyderm/v2/src/internal/middleware/version"
	"github.com/pachyderm/pachyderm/v2/src/internal/migrations"
	"github.com/pachyderm/pachyderm/v2/src/internal/profileutil"
	"github.com/pachyderm/pachyderm/v2/src/internal/serviceenv"
	"github.com/pachyderm/pachyderm/v2/src/internal/tls"
	"github.com/pachyderm/pachyderm/v2/src/internal/tracing"
	txnenv "github.com/pachyderm/pachyderm/v2/src/internal/transactionenv"
	licenseclient "github.com/pachyderm/pachyderm/v2/src/license"
	pfsclient "github.com/pachyderm/pachyderm/v2/src/pfs"
	ppsclient "github.com/pachyderm/pachyderm/v2/src/pps"
	proxyclient "github.com/pachyderm/pachyderm/v2/src/proxy"
	adminserver "github.com/pachyderm/pachyderm/v2/src/server/admin/server"
	authserver "github.com/pachyderm/pachyderm/v2/src/server/auth/server"
	debugserver "github.com/pachyderm/pachyderm/v2/src/server/debug/server"
	eprsserver "github.com/pachyderm/pachyderm/v2/src/server/enterprise/server"
	proxyserver "github.com/pachyderm/pachyderm/v2/src/server/proxy/server"
	"google.golang.org/grpc/health"

	identity_server "github.com/pachyderm/pachyderm/v2/src/server/identity/server"
	licenseserver "github.com/pachyderm/pachyderm/v2/src/server/license/server"
	"github.com/pachyderm/pachyderm/v2/src/server/pfs/s3"
	pfs_server "github.com/pachyderm/pachyderm/v2/src/server/pfs/server"
	pps_server "github.com/pachyderm/pachyderm/v2/src/server/pps/server"
	txnserver "github.com/pachyderm/pachyderm/v2/src/server/transaction/server"
	transactionclient "github.com/pachyderm/pachyderm/v2/src/transaction"
	"github.com/pachyderm/pachyderm/v2/src/version"
	"github.com/pachyderm/pachyderm/v2/src/version/versionpb"
	"go.uber.org/automaxprocs/maxprocs"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
	flag "github.com/spf13/pflag"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health/grpc_health_v1"
)

var mode string
var readiness bool

func init() {
	flag.StringVar(&mode, "mode", "full", "Pachd currently supports three modes: full, enterprise and sidecar. full includes everything you need in a full pachd node. Enterprise runs the Enterprise Server. Sidecar runs only PFS, the Auth service, and a stripped-down version of PPS.")
	flag.BoolVar(&readiness, "readiness", false, "Run readiness check.")
	flag.Parse()
}

func main() {
	log.SetFormatter(logutil.FormatterFunc(logutil.Pretty))
	maxprocs.Set(maxprocs.Logger(log.Printf))

	switch {
	case readiness:
		cmdutil.Main(doReadinessCheck, &serviceenv.GlobalConfiguration{})
	case mode == "full":
		cmdutil.Main(doFullMode, &serviceenv.PachdFullConfiguration{})
	case mode == "enterprise":
		cmdutil.Main(doEnterpriseMode, &serviceenv.GlobalConfiguration{})
	case mode == "sidecar":
		cmdutil.Main(doSidecarMode, &serviceenv.PachdFullConfiguration{})
	default:
		fmt.Printf("unrecognized mode: %s\n", mode)
	}
}

func doReadinessCheck(config interface{}) error {
	env := serviceenv.InitPachOnlyEnv(serviceenv.NewConfiguration(config))
	return env.GetPachClient(context.Background()).Health()
}

func doEnterpriseMode(config interface{}) (retErr error) {
	defer func() {
		if retErr != nil {
			log.WithError(retErr).Print("failed to start server")
			pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		}
	}()
	switch logLevel := os.Getenv("LOG_LEVEL"); logLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "info", "":
		log.SetLevel(log.InfoLevel)
	default:
		log.Errorf("Unrecognized log level %s, falling back to default of \"info\"", logLevel)
		log.SetLevel(log.InfoLevel)
	}
	// must run InstallJaegerTracer before InitWithKube (otherwise InitWithKube
	// may create a pach client before tracing is active, not install the Jaeger
	// gRPC interceptor in the client, and not propagate traces)
	if endpoint := tracing.InstallJaegerTracerFromEnv(); endpoint != "" {
		log.Printf("connecting to Jaeger at %q", endpoint)
	} else {
		log.Printf("no Jaeger collector found (JAEGER_COLLECTOR_SERVICE_HOST not set)")
	}
	env := serviceenv.InitWithKube(serviceenv.NewConfiguration(config))
	profileutil.StartCloudProfiler("pachyderm-pachd-enterprise", env.Config())
	debug.SetGCPercent(env.Config().GCPercent)

	// TODO: currently all pachds attempt to apply migrations, we should coordinate this
	if err := dbutil.WaitUntilReady(context.Background(), log.StandardLogger(), env.GetDBClient()); err != nil {
		return err
	}
	if err := migrations.ApplyMigrations(context.Background(), env.GetDBClient(), migrations.MakeEnv(nil, env.GetEtcdClient()), clusterstate.DesiredClusterState); err != nil {
		return err
	}
	if err := migrations.BlockUntil(context.Background(), env.GetDBClient(), clusterstate.DesiredClusterState); err != nil {
		return err
	}
	env.InitDexDB()

	if env.Config().EtcdPrefix == "" {
		env.Config().EtcdPrefix = col.DefaultPrefix
	}

	// Setup External Pachd GRPC Server.
	authInterceptor := authmw.NewInterceptor(env.AuthServer)
	externalServer, err := grpcutil.NewServer(
		context.Background(),
		true,
		// Add an UnknownServiceHandler to catch the case where the user has a client with the wrong major version.
		// Weirdly, GRPC seems to run the interceptor stack before the UnknownServiceHandler, so this is never called
		// (because the version_middleware interceptor throws an error, or the auth interceptor does).
		grpc.UnknownServiceHandler(func(srv interface{}, stream grpc.ServerStream) error {
			method, _ := grpc.MethodFromServerStream(stream)
			return fmt.Errorf("unknown service %v", method)
		}),
		grpc.ChainUnaryInterceptor(
			version_middleware.UnaryServerInterceptor,
			tracing.UnaryServerInterceptor(),
			authInterceptor.InterceptUnary,
		),
		grpc.ChainStreamInterceptor(
			version_middleware.StreamServerInterceptor,
			tracing.StreamServerInterceptor(),
			authInterceptor.InterceptStream,
		),
	)
	if err != nil {
		return err
	}

	if err := logGRPCServerSetup("External Enterprise Server", func() error {
		txnEnv := txnenv.New()
		if err := logGRPCServerSetup("Auth API", func() error {
			authAPIServer, err := authserver.NewAuthServer(
				authserver.EnvFromServiceEnv(env, txnEnv),
				true,
				true,
				true,
			)
			if err != nil {
				return err
			}
			authclient.RegisterAPIServer(externalServer.Server, authAPIServer)
			env.SetAuthServer(authAPIServer)
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("License API", func() error {
			licenseAPIServer, err := licenseserver.New(licenseserver.EnvFromServiceEnv(env))
			if err != nil {
				return err
			}
			licenseclient.RegisterAPIServer(externalServer.Server, licenseAPIServer)
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Enterprise API", func() error {
			enterpriseAPIServer, err := eprsserver.NewEnterpriseServer(
				eprsserver.EnvFromServiceEnv(env, path.Join(env.Config().EtcdPrefix, env.Config().EnterpriseEtcdPrefix), txnEnv),
				true,
			)
			if err != nil {
				return err
			}
			eprsclient.RegisterAPIServer(externalServer.Server, enterpriseAPIServer)
			env.SetEnterpriseServer(enterpriseAPIServer)
			return nil
		}); err != nil {
			return err
		}

		healthServer := health.NewServer()
		healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		if err := logGRPCServerSetup("Health", func() error {
			grpc_health_v1.RegisterHealthServer(externalServer.Server, healthServer)
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Admin API", func() error {
			adminclient.RegisterAPIServer(externalServer.Server, adminserver.NewAPIServer(adminserver.EnvFromServiceEnv(env)))
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Version API", func() error {
			versionpb.RegisterAPIServer(externalServer.Server, version.NewAPIServer(version.Version, version.APIServerOptions{}))
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Identity API", func() error {
			idAPIServer := identity_server.NewIdentityServer(
				identity_server.EnvFromServiceEnv(env),
				true,
			)
			identityclient.RegisterAPIServer(externalServer.Server, idAPIServer)
			return nil
		}); err != nil {
			return err
		}
		txnEnv.Initialize(env, nil)
		if _, err := externalServer.ListenTCP("", env.Config().Port); err != nil {
			return err
		}
		healthServer.Resume()
		return nil
	}); err != nil {
		return err
	}

	// Setup Internal Pachd GRPC Server.
	internalServer, err := grpcutil.NewServer(context.Background(), false, grpc.ChainUnaryInterceptor(tracing.UnaryServerInterceptor(), authInterceptor.InterceptUnary), grpc.StreamInterceptor(authInterceptor.InterceptStream))
	if err != nil {
		return err
	}

	if err := logGRPCServerSetup("Internal Enterprise Server", func() error {
		txnEnv := txnenv.New()
		if err := logGRPCServerSetup("Auth API", func() error {
			authAPIServer, err := authserver.NewAuthServer(
				authserver.EnvFromServiceEnv(env, txnEnv),
				false,
				false,
				true,
			)
			if err != nil {
				return err
			}
			authclient.RegisterAPIServer(internalServer.Server, authAPIServer)
			env.SetAuthServer(authAPIServer)
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("License API", func() error {
			licenseAPIServer, err := licenseserver.New(licenseserver.EnvFromServiceEnv(env))
			if err != nil {
				return err
			}
			licenseclient.RegisterAPIServer(internalServer.Server, licenseAPIServer)
			return nil
		}); err != nil {
			return err
		}

		healthServer := health.NewServer()
		healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		if err := logGRPCServerSetup("Health", func() error {
			grpc_health_v1.RegisterHealthServer(internalServer.Server, healthServer)
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Enterprise API", func() error {
			enterpriseAPIServer, err := eprsserver.NewEnterpriseServer(
				eprsserver.EnvFromServiceEnv(env, path.Join(env.Config().EtcdPrefix, env.Config().EnterpriseEtcdPrefix), txnEnv),
				false,
			)
			if err != nil {
				return err
			}
			eprsclient.RegisterAPIServer(internalServer.Server, enterpriseAPIServer)
			env.SetEnterpriseServer(enterpriseAPIServer)
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Admin API", func() error {
			adminclient.RegisterAPIServer(internalServer.Server, adminserver.NewAPIServer(adminserver.EnvFromServiceEnv(env)))
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Version API", func() error {
			versionpb.RegisterAPIServer(internalServer.Server, version.NewAPIServer(version.Version, version.APIServerOptions{}))
			return nil
		}); err != nil {
			return err
		}

		if err := logGRPCServerSetup("Identity API", func() error {
			idAPIServer := identity_server.NewIdentityServer(
				identity_server.EnvFromServiceEnv(env),
				false,
			)
			identityclient.RegisterAPIServer(internalServer.Server, idAPIServer)
			return nil
		}); err != nil {
			return err
		}
		txnEnv.Initialize(env, nil)
		if _, err := internalServer.ListenTCP("", env.Config().PeerPort); err != nil {
			return err
		}
		healthServer.Resume()
		return nil
	}); err != nil {
		return err
	}

	// Create the goroutines for the servers.
	// Any server error is considered critical and will cause Pachd to exit.
	// The first server that errors will have its error message logged.
	errChan := make(chan error, 1)
	go waitForError("External Enterprise GRPC Server", errChan, true, func() error {
		return externalServer.Wait()
	})
	go waitForError("Internal Enterprise GRPC Server", errChan, true, func() error {
		return internalServer.Wait()
	})
	return <-errChan
}

func doSidecarMode(config interface{}) (retErr error) {
	defer func() {
		if retErr != nil {
			pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		}
	}()
	switch logLevel := os.Getenv("LOG_LEVEL"); logLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "info", "":
		log.SetLevel(log.InfoLevel)
	default:
		log.Errorf("Unrecognized log level %s, falling back to default of \"info\"", logLevel)
		log.SetLevel(log.InfoLevel)
	}
	// must run InstallJaegerTracer before InitWithKube (otherwise InitWithKube
	// may create a pach client before tracing is active, not install the Jaeger
	// gRPC interceptor in the client, and not propagate traces)
	if endpoint := tracing.InstallJaegerTracerFromEnv(); endpoint != "" {
		log.Printf("connecting to Jaeger at %q", endpoint)
	} else {
		log.Printf("no Jaeger collector found (JAEGER_COLLECTOR_SERVICE_HOST not set)")
	}
	env := serviceenv.InitWithKube(serviceenv.NewConfiguration(config))
	profileutil.StartCloudProfiler("pachyderm-pachd-sidecar", env.Config())
	debug.SetGCPercent(env.Config().GCPercent)
	if env.Config().EtcdPrefix == "" {
		env.Config().EtcdPrefix = col.DefaultPrefix
	}
	authInterceptor := authmw.NewInterceptor(env.AuthServer)
	server, err := grpcutil.NewServer(
		context.Background(),
		false,
		grpc.ChainUnaryInterceptor(
			tracing.UnaryServerInterceptor(),
			authInterceptor.InterceptUnary,
		),
		grpc.ChainStreamInterceptor(
			tracing.StreamServerInterceptor(),
			authInterceptor.InterceptStream,
		),
	)
	if err != nil {
		return err
	}
	txnEnv := txnenv.New()
	if err := logGRPCServerSetup("Auth API", func() error {
		authAPIServer, err := authserver.NewAuthServer(
			authserver.EnvFromServiceEnv(env, txnEnv),
			false,
			false,
			false,
		)
		if err != nil {
			return err
		}
		authclient.RegisterAPIServer(server.Server, authAPIServer)
		env.SetAuthServer(authAPIServer)
		return nil
	}); err != nil {
		return err
	}
	if err := logGRPCServerSetup("PFS API", func() error {
		pfsEnv, err := pfs_server.EnvFromServiceEnv(env, txnEnv)
		if err != nil {
			return err
		}
		pfsAPIServer, err := pfs_server.NewSidecarAPIServer(*pfsEnv)
		if err != nil {
			return err
		}
		pfsclient.RegisterAPIServer(server.Server, pfsAPIServer)
		env.SetPfsServer(pfsAPIServer)
		return nil
	}); err != nil {
		return err
	}
	if err := logGRPCServerSetup("PPS API", func() error {
		ppsAPIServer, err := pps_server.NewSidecarAPIServer(
			pps_server.EnvFromServiceEnv(env, txnEnv, nil),
			env.Config().Namespace,
			env.Config().PPSWorkerPort,
			env.Config().PeerPort,
		)
		if err != nil {
			return err
		}
		ppsclient.RegisterAPIServer(server.Server, ppsAPIServer)
		env.SetPpsServer(ppsAPIServer)
		return nil
	}); err != nil {
		return err
	}
	if err := logGRPCServerSetup("Enterprise API", func() error {
		enterpriseAPIServer, err := eprsserver.NewEnterpriseServer(
			eprsserver.EnvFromServiceEnv(env, path.Join(env.Config().EtcdPrefix, env.Config().EnterpriseEtcdPrefix), txnEnv),
			false,
		)
		if err != nil {
			return err
		}
		eprsclient.RegisterAPIServer(server.Server, enterpriseAPIServer)
		env.SetEnterpriseServer(enterpriseAPIServer)
		return nil
	}); err != nil {
		return err
	}
	var transactionAPIServer txnserver.APIServer
	if err := logGRPCServerSetup("Transaction API", func() error {
		transactionAPIServer, err = txnserver.NewAPIServer(
			env,
			txnEnv,
		)
		if err != nil {
			return err
		}
		transactionclient.RegisterAPIServer(server.Server, transactionAPIServer)
		return nil
	}); err != nil {
		return err
	}
	if err := logGRPCServerSetup("Health", func() error {
		healthServer := health.NewServer()
		grpc_health_v1.RegisterHealthServer(server.Server, healthServer)
		return nil
	}); err != nil {
		return err
	}
	if err := logGRPCServerSetup("Debug", func() error {
		debugclient.RegisterDebugServer(server.Server, debugserver.NewDebugServer(
			env,
			env.Config().PachdPodName,
			nil,
		))
		return nil
	}); err != nil {
		return err
	}
	txnEnv.Initialize(env, transactionAPIServer)
	// The sidecar only needs to serve traffic on the peer port, as it only serves
	// traffic from the user container (the worker binary and occasionally user
	// pipelines)
	if _, err := server.ListenTCP("", env.Config().PeerPort); err != nil {
		return err
	}
	return server.Wait()
}

func doFullMode(config interface{}) (retErr error) {
	defer func() {
		if retErr != nil {
			pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		}
	}()
	switch logLevel := os.Getenv("LOG_LEVEL"); logLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	case "info", "":
		log.SetLevel(log.InfoLevel)
	default:
		log.Errorf("Unrecognized log level %s, falling back to default of \"info\"", logLevel)
		log.SetLevel(log.InfoLevel)
	}

	// must run InstallJaegerTracer before InitWithKube/pach client initialization
	if endpoint := tracing.InstallJaegerTracerFromEnv(); endpoint != "" {
		log.Printf("connecting to Jaeger at %q", endpoint)
	} else {
		log.Printf("no Jaeger collector found (JAEGER_COLLECTOR_SERVICE_HOST not set)")
	}
	env := serviceenv.InitWithKube(serviceenv.NewConfiguration(config))
	profileutil.StartCloudProfiler("pachyderm-pachd-full", env.Config())
	debug.SetGCPercent(env.Config().GCPercent)
	if env.Config().EtcdPrefix == "" {
		env.Config().EtcdPrefix = col.DefaultPrefix
	}

	// TODO: currently all pachds attempt to apply migrations, we should coordinate this
	if err := dbutil.WaitUntilReady(context.Background(), log.StandardLogger(), env.GetDBClient()); err != nil {
		return err
	}
	if err := migrations.ApplyMigrations(context.Background(), env.GetDBClient(), migrations.MakeEnv(nil, env.GetEtcdClient()), clusterstate.DesiredClusterState); err != nil {
		return err
	}
	if err := migrations.BlockUntil(context.Background(), env.GetDBClient(), clusterstate.DesiredClusterState); err != nil {
		return err
	}
	env.InitDexDB()

	var reporter *metrics.Reporter
	if env.Config().Metrics {
		reporter = metrics.NewReporter(env)
	}
	requireNoncriticalServers := !env.Config().RequireCriticalServersOnly

	// Setup External Pachd GRPC Server.
	authInterceptor := authmw.NewInterceptor(env.AuthServer)
	externalServer, err := grpcutil.NewServer(
		context.Background(),
		true,
		// Add an UnknownServiceHandler to catch the case where the user has a client with the wrong major version.
		// Weirdly, GRPC seems to run the interceptor stack before the UnknownServiceHandler, so this is never called
		// (because the version_middleware interceptor throws an error, or the auth interceptor does).
		grpc.UnknownServiceHandler(func(srv interface{}, stream grpc.ServerStream) error {
			method, _ := grpc.MethodFromServerStream(stream)
			return fmt.Errorf("unknown service %v", method)
		}),
		grpc.ChainUnaryInterceptor(
			version_middleware.UnaryServerInterceptor,
			tracing.UnaryServerInterceptor(),
			authInterceptor.InterceptUnary,
		),
		grpc.ChainStreamInterceptor(
			version_middleware.StreamServerInterceptor,
			tracing.StreamServerInterceptor(),
			authInterceptor.InterceptStream,
		),
	)

	if err != nil {
		return err
	}
	if err := logGRPCServerSetup("External Pachd", func() error {
		txnEnv := txnenv.New()

		if err := logGRPCServerSetup("Identity API", func() error {
			idAPIServer := identity_server.NewIdentityServer(
				identity_server.EnvFromServiceEnv(env),
				true,
			)
			identityclient.RegisterAPIServer(externalServer.Server, idAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Auth API", func() error {
			authAPIServer, err := authserver.NewAuthServer(
				authserver.EnvFromServiceEnv(env, txnEnv),
				true, requireNoncriticalServers, true,
			)
			if err != nil {
				return err
			}
			authclient.RegisterAPIServer(externalServer.Server, authAPIServer)
			env.SetAuthServer(authAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("PFS API", func() error {
			pfsEnv, err := pfs_server.EnvFromServiceEnv(env, txnEnv)
			if err != nil {
				return err
			}
			pfsAPIServer, err := pfs_server.NewAPIServer(*pfsEnv)
			if err != nil {
				return err
			}
			pfsclient.RegisterAPIServer(externalServer.Server, pfsAPIServer)
			env.SetPfsServer(pfsAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("PPS API", func() error {
			ppsAPIServer, err := pps_server.NewAPIServer(
				pps_server.EnvFromServiceEnv(env, txnEnv, reporter),
			)
			if err != nil {
				return err
			}
			ppsclient.RegisterAPIServer(externalServer.Server, ppsAPIServer)
			env.SetPpsServer(ppsAPIServer)
			return nil
		}); err != nil {
			return err
		}
		var transactionAPIServer txnserver.APIServer
		if err := logGRPCServerSetup("Transaction API", func() error {
			transactionAPIServer, err = txnserver.NewAPIServer(
				env,
				txnEnv,
			)
			if err != nil {
				return err
			}
			transactionclient.RegisterAPIServer(externalServer.Server, transactionAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Enterprise API", func() error {
			enterpriseAPIServer, err := eprsserver.NewEnterpriseServer(
				eprsserver.EnvFromServiceEnv(env, path.Join(env.Config().EtcdPrefix, env.Config().EnterpriseEtcdPrefix), txnEnv),
				true,
			)
			if err != nil {
				return err
			}
			eprsclient.RegisterAPIServer(externalServer.Server, enterpriseAPIServer)
			env.SetEnterpriseServer(enterpriseAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("License API", func() error {
			licenseAPIServer, err := licenseserver.New(licenseserver.EnvFromServiceEnv(env))
			if err != nil {
				return err
			}
			licenseclient.RegisterAPIServer(externalServer.Server, licenseAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Admin API", func() error {
			adminclient.RegisterAPIServer(externalServer.Server, adminserver.NewAPIServer(adminserver.EnvFromServiceEnv(env)))
			return nil
		}); err != nil {
			return err
		}
		healthServer := health.NewServer()
		healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		if err := logGRPCServerSetup("Health", func() error {
			grpc_health_v1.RegisterHealthServer(externalServer.Server, healthServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Version API", func() error {
			versionpb.RegisterAPIServer(externalServer.Server, version.NewAPIServer(version.Version, version.APIServerOptions{}))
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Debug", func() error {
			debugclient.RegisterDebugServer(externalServer.Server, debugserver.NewDebugServer(
				env,
				env.Config().PachdPodName,
				nil,
			))
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Proxy API", func() error {
			proxyclient.RegisterAPIServer(externalServer.Server, proxyserver.NewAPIServer(proxyserver.Env{
				Listener: env.GetPostgresListener(),
			}))
			return nil
		}); err != nil {
			return err
		}
		txnEnv.Initialize(env, transactionAPIServer)
		if _, err := externalServer.ListenTCP("", env.Config().Port); err != nil {
			return err
		}
		healthServer.Resume()
		return nil
	}); err != nil {
		return err
	}
	// Setup Internal Pachd GRPC Server.
	internalServer, err := grpcutil.NewServer(context.Background(), false, grpc.ChainUnaryInterceptor(tracing.UnaryServerInterceptor(), authInterceptor.InterceptUnary), grpc.StreamInterceptor(authInterceptor.InterceptStream))
	if err != nil {
		return err
	}
	if err := logGRPCServerSetup("Internal Pachd", func() error {
		txnEnv := txnenv.New()
		if err := logGRPCServerSetup("PFS API", func() error {
			pfsEnv, err := pfs_server.EnvFromServiceEnv(env, txnEnv)
			if err != nil {
				return err
			}
			pfsAPIServer, err := pfs_server.NewAPIServer(
				*pfsEnv,
			)
			if err != nil {
				return err
			}
			pfsclient.RegisterAPIServer(internalServer.Server, pfsAPIServer)
			env.SetPfsServer(pfsAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("PPS API", func() error {
			ppsAPIServer, err := pps_server.NewAPIServer(
				pps_server.EnvFromServiceEnv(env, txnEnv, reporter),
			)
			if err != nil {
				return err
			}
			ppsclient.RegisterAPIServer(internalServer.Server, ppsAPIServer)
			env.SetPpsServer(ppsAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Identity API", func() error {
			idAPIServer := identity_server.NewIdentityServer(
				identity_server.EnvFromServiceEnv(env),
				false,
			)
			identityclient.RegisterAPIServer(internalServer.Server, idAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Auth API", func() error {
			authAPIServer, err := authserver.NewAuthServer(
				authserver.EnvFromServiceEnv(env, txnEnv),
				false,
				requireNoncriticalServers,
				true,
			)
			if err != nil {
				return err
			}
			authclient.RegisterAPIServer(internalServer.Server, authAPIServer)
			env.SetAuthServer(authAPIServer)
			return nil
		}); err != nil {
			return err
		}
		var transactionAPIServer txnserver.APIServer
		if err := logGRPCServerSetup("Transaction API", func() error {
			transactionAPIServer, err = txnserver.NewAPIServer(
				env,
				txnEnv,
			)
			if err != nil {
				return err
			}
			transactionclient.RegisterAPIServer(internalServer.Server, transactionAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("License API", func() error {
			licenseAPIServer, err := licenseserver.New(
				licenseserver.EnvFromServiceEnv(env),
			)
			if err != nil {
				return err
			}
			licenseclient.RegisterAPIServer(internalServer.Server, licenseAPIServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Enterprise API", func() error {
			enterpriseAPIServer, err := eprsserver.NewEnterpriseServer(
				eprsserver.EnvFromServiceEnv(env, path.Join(env.Config().EtcdPrefix, env.Config().EnterpriseEtcdPrefix), txnEnv),
				false,
			)
			if err != nil {
				return err
			}
			eprsclient.RegisterAPIServer(internalServer.Server, enterpriseAPIServer)
			env.SetEnterpriseServer(enterpriseAPIServer)
			return nil
		}); err != nil {
			return err
		}
		healthServer := health.NewServer()
		healthServer.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
		if err := logGRPCServerSetup("Health", func() error {
			grpc_health_v1.RegisterHealthServer(internalServer.Server, healthServer)
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Version API", func() error {
			versionpb.RegisterAPIServer(internalServer.Server, version.NewAPIServer(version.Version, version.APIServerOptions{}))
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Admin API", func() error {
			adminclient.RegisterAPIServer(internalServer.Server, adminserver.NewAPIServer(adminserver.EnvFromServiceEnv(env)))
			return nil
		}); err != nil {
			return err
		}
		if err := logGRPCServerSetup("Proxy API", func() error {
			proxyclient.RegisterAPIServer(internalServer.Server, proxyserver.NewAPIServer(proxyserver.Env{
				Listener: env.GetPostgresListener(),
			}))
			return nil
		}); err != nil {
			return err
		}
		txnEnv.Initialize(env, transactionAPIServer)
		if _, err := internalServer.ListenTCP("", env.Config().PeerPort); err != nil {
			return err
		}
		healthServer.Resume()
		return nil
	}); err != nil {
		return err
	}
	// Create the goroutines for the servers.
	// Any server error is considered critical and will cause Pachd to exit.
	// The first server that errors will have its error message logged.
	errChan := make(chan error, 1)
	go waitForError("External Pachd GRPC Server", errChan, true, func() error {
		return externalServer.Wait()
	})
	go waitForError("Internal Pachd GRPC Server", errChan, true, func() error {
		return internalServer.Wait()
	})
	go waitForError("S3 Server", errChan, requireNoncriticalServers, func() error {
		router := s3.Router(s3.NewMasterDriver(), func() (*client.APIClient, error) {
			return env.GetPachClient(context.Background()), nil
		})
		server := s3.Server(env.Config().S3GatewayPort, router)
		certPath, keyPath, err := tls.GetCertPaths()
		if err != nil {
			log.Warnf("s3gateway TLS disabled: %v", err)
			return server.ListenAndServe()
		}
		cLoader := tls.NewCertLoader(certPath, keyPath, tls.CertCheckFrequency)
		// Read TLS cert and key
		err = cLoader.LoadAndStart()
		if err != nil {
			return errors.Wrapf(err, "couldn't load TLS cert for s3gateway: %v", err)
		}
		server.TLSConfig = &gotls.Config{GetCertificate: cLoader.GetCertificate}
		return server.ListenAndServeTLS(certPath, keyPath)
	})
	go waitForError("Prometheus Server", errChan, requireNoncriticalServers, func() error {
		http.Handle("/metrics", promhttp.Handler())
		return http.ListenAndServe(fmt.Sprintf(":%v", env.Config().PrometheusPort), nil)
	})
	return <-errChan
}

func logGRPCServerSetup(name string, f func() error) (retErr error) {
	log.Printf("started setting up %v GRPC Server", name)
	defer func() {
		if retErr != nil {
			retErr = errors.Wrapf(retErr, "error setting up %v GRPC Server", name)
		} else {
			log.Printf("finished setting up %v GRPC Server", name)
		}
	}()
	return f()
}

func waitForError(name string, errChan chan error, required bool, f func() error) {
	if err := f(); !errors.Is(err, http.ErrServerClosed) {
		if !required {
			log.Errorf("error setting up and/or running %v: %v", name, err)
		} else {
			errChan <- errors.Wrapf(err, "error setting up and/or running %v (use --require-critical-servers-only deploy flag to ignore errors from noncritical servers)", name)
		}
	}
}
