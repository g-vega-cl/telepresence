package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/telepresenceio/telepresence/rpc/v2/connector"
	"github.com/telepresenceio/telepresence/rpc/v2/manager"
	"github.com/telepresenceio/telepresence/v2/pkg/client"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/cliutil"
	"github.com/telepresenceio/telepresence/v2/pkg/client/cli/output"
)

func xlistCommand() *cobra.Command {
	return &cobra.Command{
		Use:  "xlist",
		Args: cobra.NoArgs,

		Short: "List all intercepts known to the Traffic Manager",
		Long: "" +
			"Unlike `telepresence list`, this also returns intercepts owned by " +
			"different users, rather than just your own intercepts.  You must have " +
			"called `telepresence connect` before calling this.",

		RunE: func(cmd *cobra.Command, _ []string) error {
			return cliutil.WithManager(cmd.Context(), func(ctx context.Context, managerClient manager.ManagerClient) error {
				watchClient, err := managerClient.WatchIntercepts(ctx, &manager.SessionInfo{})
				if err != nil {
					return err
				}
				snapshot, err := watchClient.Recv()
				if err != nil {
					return err
				}
				if err := watchClient.CloseSend(); err != nil {
					return err
				}
				fmt.Println(DescribeIntercepts(snapshot.Intercepts, nil, true))

				return nil
			})
		},
	}
}

type listInfo struct {
	onlyIntercepts    bool
	onlyAgents        bool
	onlyInterceptable bool
	debug             bool
	namespace         string
	watch             bool
}

func listCommand() *cobra.Command {
	s := &listInfo{}
	cmd := &cobra.Command{
		Use:  "list",
		Args: cobra.NoArgs,

		Short: "List current intercepts",
		RunE:  s.list,
	}
	flags := cmd.Flags()
	flags.BoolVarP(&s.onlyIntercepts, "intercepts", "i", false, "intercepts only")
	flags.BoolVarP(&s.onlyAgents, "agents", "a", false, "with installed agents only")
	flags.BoolVarP(&s.onlyInterceptable, "only-interceptable", "o", true, "interceptable workloads only")
	flags.BoolVar(&s.debug, "debug", false, "include debugging information")
	flags.StringVarP(&s.namespace, "namespace", "n", "", "If present, the namespace scope for this CLI request")
	flags.BoolVarP(&s.watch, "watch", "w", false, "watch a namespace. --agents and --intercepts are disabled if this flag is set")
	return cmd
}

// list requests a list current intercepts from the daemon
func (s *listInfo) list(cmd *cobra.Command, _ []string) error {
	stdout := cmd.OutOrStdout()
	return withConnector(cmd, true, nil, func(ctx context.Context, cs *connectorState) error {
		var filter connector.ListRequest_Filter
		switch {
		case s.onlyIntercepts:
			filter = connector.ListRequest_INTERCEPTS
		case s.onlyAgents:
			filter = connector.ListRequest_INSTALLED_AGENTS
		case s.onlyInterceptable:
			filter = connector.ListRequest_INTERCEPTABLE
		default:
			filter = connector.ListRequest_EVERYTHING
		}

		cfg := client.GetConfig(ctx)
		maxRecSize := int64(1024 * 1024 * 20) // Default to 20 Mb here. List can be quit long.
		if !cfg.Grpc.MaxReceiveSize.IsZero() {
			if mz, ok := cfg.Grpc.MaxReceiveSize.AsInt64(); ok {
				if mz > maxRecSize {
					maxRecSize = mz
				}
			}
		}

		jsonOut := output.WantsJSONOutput(cmd.Flags())
		if !s.watch {
			r, err := cs.userD.List(ctx, &connector.ListRequest{Filter: filter, Namespace: s.namespace}, grpc.MaxCallRecvMsgSize(int(maxRecSize)))
			if err != nil {
				return err
			}
			s.printList(r.Workloads, stdout, jsonOut)
			return nil
		}

		stream, err := cs.userD.WatchWorkloads(ctx, &connector.WatchWorkloadsRequest{Namespaces: []string{s.namespace}}, grpc.MaxCallRecvMsgSize(int(maxRecSize)))
		if err != nil {
			return err
		}

		ch := make(chan *connector.WorkloadInfoSnapshot)
		go func() {
			for {
				r, err := stream.Recv()
				if err != nil {
					break
				}
				ch <- r
			}
		}()

	looper:
		for {
			select {
			case r := <-ch:
				s.printList(r.Workloads, stdout, jsonOut)
			case <-ctx.Done():
				break looper
			}
		}
		return nil
	})
}

func (s *listInfo) printList(workloads []*connector.WorkloadInfo, stdout io.Writer, jsonOut bool) {
	var streamerOut output.StructuredStreamer

	if jsonOut {
		streamerOut, _ = stdout.(output.StructuredStreamer)
		if streamerOut == nil {
			panic("writer not output.StructuredStreamer")
		}
	}

	if len(workloads) == 0 {
		if jsonOut {
			streamerOut.StructuredStream([]struct{}{}, nil)
		} else {
			fmt.Fprintln(stdout, "No Workloads (Deployments, StatefulSets, or ReplicaSets)")
		}
		return
	}

	state := func(workload *connector.WorkloadInfo) string {
		if iis := workload.InterceptInfos; len(iis) > 0 {
			return DescribeIntercepts(iis, nil, s.debug)
		}
		ai := workload.AgentInfo
		if ai != nil {
			return "ready to intercept (traffic-agent already installed)"
		}
		if workload.NotInterceptableReason != "" {
			return "not interceptable (traffic-agent not installed): " + workload.NotInterceptableReason
		} else {
			return "ready to intercept (traffic-agent not yet installed)"
		}
	}

	if jsonOut {
		streamerOut.StructuredStream(workloads, nil)
	} else {
		includeNs := false
		ns := s.namespace
		for _, dep := range workloads {
			depNs := dep.Namespace
			if depNs == "" {
				// Local-only, so use namespace of first intercept
				depNs = dep.InterceptInfos[0].Spec.Namespace
			}
			if ns != "" && depNs != ns {
				includeNs = true
				break
			}
			ns = depNs
		}
		nameLen := 0
		for _, dep := range workloads {
			n := dep.Name
			if n == "" {
				// Local-only, so use name of first intercept
				n = dep.InterceptInfos[0].Spec.Name
			}
			nl := len(n)
			if includeNs {
				nl += len(dep.Namespace) + 1
			}
			if nl > nameLen {
				nameLen = nl
			}
		}
		for _, workload := range workloads {
			if workload.Name == "" {
				// Local-only, so use name of first intercept
				n := workload.InterceptInfos[0].Spec.Name
				if includeNs {
					n += "." + workload.Namespace
				}
				fmt.Fprintf(stdout, "%-*s: local-only intercept\n", nameLen, n)
			} else {
				n := workload.Name
				if includeNs {
					n += "." + workload.Namespace
				}
				fmt.Fprintf(stdout, "%-*s: %s\n", nameLen, n, state(workload))
			}
		}
	}
}

func DescribeIntercepts(iis []*manager.InterceptInfo, volumeMountsPrevented error, debug bool) string {
	sb := strings.Builder{}
	sb.WriteString("intercepted")
	for i, ii := range iis {
		if i > 0 {
			sb.WriteByte('\n')
		}
		describeIntercept(ii, volumeMountsPrevented, debug, &sb)
	}
	return sb.String()
}

func describeIntercept(ii *manager.InterceptInfo, volumeMountsPrevented error, debug bool, sb *strings.Builder) {
	type kv struct {
		Key   string
		Value string
	}

	var fields []kv

	fields = append(fields, kv{"Intercept name", ii.Spec.Name})
	fields = append(fields, kv{"State", func() string {
		msg := ""
		if ii.Disposition > manager.InterceptDispositionType_WAITING {
			msg += "error: "
		}
		msg += ii.Disposition.String()
		if ii.Message != "" {
			msg += ": " + ii.Message
		}
		return msg
	}()})
	fields = append(fields, kv{"Workload kind", ii.Spec.WorkloadKind})

	if debug {
		fields = append(fields, kv{"ID", ii.Id})
	}

	fields = append(fields, kv{"Destination",
		net.JoinHostPort(ii.Spec.TargetHost, fmt.Sprintf("%d", ii.Spec.TargetPort))})

	if ii.Spec.ServicePortIdentifier != "" {
		fields = append(fields, kv{"Service Port Identifier", ii.Spec.ServicePortIdentifier})
	}
	if debug {
		fields = append(fields, kv{"Mechanism", ii.Spec.Mechanism})
		fields = append(fields, kv{"Mechanism Args", fmt.Sprintf("%q", ii.Spec.MechanismArgs)})
		fields = append(fields, kv{"Metadata", fmt.Sprintf("%q", ii.Metadata)})
	}

	if ii.ClientMountPoint != "" {
		fields = append(fields, kv{"Volume Mount Point", ii.ClientMountPoint})
	} else if volumeMountsPrevented != nil {
		fields = append(fields, kv{"Volume Mount Error", volumeMountsPrevented.Error()})
	}
	if debug {
		fields = append(fields, kv{"Volume Mount Pod IP (for SFTP)", ii.PodIp})
		fields = append(fields, kv{"Volume Mount Pod port (for SFTP)", fmt.Sprintf("%d", ii.SftpPort)})
	}

	fields = append(fields, kv{"Intercepting", func() string {
		if ii.MechanismArgsDesc == "" {
			if len(ii.Spec.MechanismArgs) > 0 {
				return fmt.Sprintf("using mechanism=%q with args=%q", ii.Spec.Mechanism, ii.Spec.MechanismArgs)
			}
			return fmt.Sprintf("using mechanism=%q", ii.Spec.Mechanism)
		}
		return ii.MechanismArgsDesc
	}()})

	if ii.PreviewDomain != "" {
		previewURL := ii.PreviewDomain
		// Right now SystemA gives back domains with the leading "https://", but
		// let's not rely on that.
		if !strings.HasPrefix(previewURL, "https://") && !strings.HasPrefix(previewURL, "http://") {
			previewURL = "https://" + previewURL
		}
		fields = append(fields, kv{"Preview URL", previewURL})
	}
	if l5Hostname := ii.GetPreviewSpec().GetIngress().GetL5Host(); l5Hostname != "" {
		fields = append(fields, kv{"Layer 5 Hostname", l5Hostname})
	}

	klen := 0
	for _, kv := range fields {
		if len(kv.Key) > klen {
			klen = len(kv.Key)
		}
	}
	for _, kv := range fields {
		vlines := strings.Split(strings.TrimSpace(kv.Value), "\n")
		fmt.Fprintf(sb, "\n    %-*s: %s", klen, kv.Key, vlines[0])
		for _, vline := range vlines[1:] {
			sb.WriteString("\n      ")
			sb.WriteString(vline)
		}
	}
}
