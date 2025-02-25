package apply

import (
	goerrors "errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/utils/ptr"
	sigyaml "sigs.k8s.io/yaml"

	"github.com/akuity/kargo/internal/cli/client"
	"github.com/akuity/kargo/internal/cli/config"
	"github.com/akuity/kargo/internal/cli/option"
	kargosvcapi "github.com/akuity/kargo/pkg/api/service/v1alpha1"
)

func NewCommand(cfg config.CLIConfig, opt *option.Option) *cobra.Command {
	var filenames []string
	cmd := &cobra.Command{
		Use:   "apply [--project=project] -f (FILENAME)",
		Short: "Apply a resource from a file or from stdin",
		Example: `
# Apply a stage using the data in stage.yaml
kargo apply -f stage.yaml
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(filenames) == 0 {
				return errors.New("filename is required")
			}

			rawManifest, err := option.ReadManifests(filenames...)
			if err != nil {
				return errors.Wrap(err, "read manifests")
			}

			var printer printers.ResourcePrinter
			if ptr.Deref(opt.PrintFlags.OutputFormat, "") != "" {
				printer, err = opt.PrintFlags.ToPrinter()
				if err != nil {
					return errors.Wrap(err, "new printer")
				}
			}

			kargoSvcCli, err := client.GetClientFromConfig(ctx, cfg, opt)
			if err != nil {
				return errors.Wrap(err, "get client from config")
			}

			// TODO: Current implementation of apply is not the same as `kubectl` does.
			// It actually "replaces" resource with the given file.
			// We should provide the same implementation as `kubectl` does.
			resp, err := kargoSvcCli.CreateOrUpdateResource(ctx,
				connect.NewRequest(&kargosvcapi.CreateOrUpdateResourceRequest{
					Manifest: rawManifest,
				}))
			if err != nil {
				return errors.Wrap(err, "apply resource")
			}

			resCap := len(resp.Msg.GetResults())
			createdRes := make([]*kargosvcapi.CreateOrUpdateResourceResult_CreatedResourceManifest, 0, resCap)
			updatedRes := make([]*kargosvcapi.CreateOrUpdateResourceResult_UpdatedResourceManifest, 0, resCap)
			errs := make([]error, 0, resCap)
			for _, r := range resp.Msg.GetResults() {
				switch typedRes := r.GetResult().(type) {
				case *kargosvcapi.CreateOrUpdateResourceResult_CreatedResourceManifest:
					createdRes = append(createdRes, typedRes)
				case *kargosvcapi.CreateOrUpdateResourceResult_UpdatedResourceManifest:
					updatedRes = append(updatedRes, typedRes)
				case *kargosvcapi.CreateOrUpdateResourceResult_Error:
					errs = append(errs, errors.New(typedRes.Error))
				}
			}
			for _, r := range createdRes {
				var obj unstructured.Unstructured
				if err := sigyaml.Unmarshal(r.CreatedResourceManifest, &obj); err != nil {
					fmt.Fprintf(opt.IOStreams.ErrOut, "%s",
						errors.Wrap(err, "Error: unmarshal created manifest"))
					continue
				}
				if printer == nil {
					name := types.NamespacedName{
						Namespace: obj.GetNamespace(),
						Name:      obj.GetName(),
					}.String()
					fmt.Fprintf(opt.IOStreams.Out, "%s Created: %q\n", obj.GetKind(), name)
					continue
				}
				_ = printer.PrintObj(&obj, opt.IOStreams.Out)
			}
			for _, r := range updatedRes {
				var obj unstructured.Unstructured
				if err := sigyaml.Unmarshal(r.UpdatedResourceManifest, &obj); err != nil {
					fmt.Fprintf(opt.IOStreams.ErrOut, "%s",
						errors.Wrap(err, "Error: unmarshal updated manifest"))
					continue
				}
				if printer == nil {
					name := types.NamespacedName{
						Namespace: obj.GetNamespace(),
						Name:      obj.GetName(),
					}.String()
					fmt.Fprintf(opt.IOStreams.Out, "%s Updated: %q\n", obj.GetKind(), name)
					continue
				}
				_ = printer.PrintObj(&obj, opt.IOStreams.Out)
			}
			return goerrors.Join(errs...)
		},
	}
	opt.PrintFlags.AddFlags(cmd)
	option.Filenames(cmd.Flags(), &filenames, "apply")
	option.InsecureTLS(cmd.PersistentFlags(), opt)
	option.LocalServer(cmd.PersistentFlags(), opt)
	return cmd
}
