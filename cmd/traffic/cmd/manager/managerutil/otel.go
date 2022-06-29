package managerutil

import (
	"context"
	"os"
	"time"

	"github.com/datawire/dlib/dlog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/jaeger"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.4.0"
)

func SetupTracer(id int64, name string) (func(context.Context), error) {
	if url, ok := os.LookupEnv("OTEL_EXPORTER_JAEGER_ENDPOINT"); ok {
		// Create the Jaeger exporter
		exp, err := jaeger.New(jaeger.WithCollectorEndpoint(jaeger.WithEndpoint(url)))
		if err != nil {
			return func(context.Context) {}, err
		}
		tp := trace.NewTracerProvider(
			// Always be sure to batch in production.
			trace.WithBatcher(exp),
			trace.WithSampler(trace.AlwaysSample()),
			// Record information about this application in a Resource.
			trace.WithResource(resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceNameKey.String(name),
				attribute.Int64("ID", id),
			)),
		)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
		otel.SetTracerProvider(tp)
		return func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, time.Second*5)
			defer cancel()
			if err := tp.Shutdown(ctx); err != nil {
				dlog.Error(ctx, "error shutting down tracer: ", err)
			}
		}, nil
	}

	return func(context.Context) {}, nil
}
