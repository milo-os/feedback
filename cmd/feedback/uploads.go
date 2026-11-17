package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"go.miloapis.com/feedback/internal/attachments"
	"go.miloapis.com/feedback/internal/ratelimit"
	"go.miloapis.com/feedback/internal/uploads"
)

type uploadsOptions struct {
	miloKubeconfig string
	addr           string
	bucket         string
	maxSize        int
	perMinute      float64
	burst          int
}

func newUploadsCommand() *cobra.Command {
	o := uploadsOptions{}
	zapOpts := zap.Options{}
	cmd := &cobra.Command{
		Use:   "uploads",
		Short: "Serve attachment uploads and downloads",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))
			return runUploads(ctrl.SetupSignalHandler(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.miloKubeconfig, "milo-kubeconfig", "", "Kubeconfig for Milo. Empty uses the in-cluster config.")
	f.StringVar(&o.addr, "listen-address", ":8080", "Address to serve HTTP on. TLS is terminated in front of the service.")
	f.StringVar(&o.bucket, "attachments-bucket", "", "Bucket URL for attachments, e.g. gs://feedback-attachments or file:///tmp/attachments.")
	f.IntVar(&o.maxSize, "attachment-max-size", attachments.DefaultMaxSize, "Largest file accepted, in bytes.")
	f.Float64Var(&o.perMinute, "uploads-per-minute", 2, "Uploads a user may make per minute, on average; 0 disables the limit.")
	f.IntVar(&o.burst, "uploads-burst", 10, "Uploads a user may make at once.")
	return cmd
}

func runUploads(ctx context.Context, o uploadsOptions) error {
	log := ctrl.Log.WithName("uploads")
	if o.bucket == "" {
		return errors.New("--attachments-bucket is required")
	}
	files, err := attachments.Open(ctx, o.bucket)
	if err != nil {
		return err
	}
	defer files.Close()
	files.MaxSize = o.maxSize

	cfg, err := restConfig(o.miloKubeconfig)
	if err != nil {
		return fmt.Errorf("loading Milo config: %w", err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	objects, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	milo := uploads.Milo{Client: kube}
	s := &uploads.Server{
		Files: files, Authn: milo, Authz: milo, Objects: objects,
		Limiter: ratelimit.NewLimiter(o.perMinute, o.burst),
	}
	srv := &http.Server{
		Addr:              o.addr,
		Handler:           http.TimeoutHandler(s.Handler(), uploads.RequestTimeout, "request timed out"),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("Serving uploads", "address", o.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
