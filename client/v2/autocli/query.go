package autocli

import (
	"fmt"

	autocliv1 "cosmossdk.io/api/cosmos/autocli/v1"
	"github.com/cockroachdb/errors"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	"cosmossdk.io/client/v2/internal/strcase"
	"cosmossdk.io/client/v2/internal/util"
)

// BuildQueryCommand builds the query commands for all the provided modules. If a custom command is provided for a
// module, this is used instead of any automatically generated CLI commands. This allows apps to a fully dynamic client
// with a more customized experience if a binary with custom commands is downloaded.
func (b *Builder) BuildQueryCommand(moduleOptions map[string]*autocliv1.ModuleOptions, customCmds map[string]*cobra.Command) (*cobra.Command, error) {
	queryCmd := topLevelCmd("query", "Querying subcommands")
	queryCmd.Aliases = []string{"q"}
	if err := b.EnhanceQueryCommand(queryCmd, moduleOptions, customCmds); err != nil {
		return nil, err
	}

	return queryCmd, nil
}

// EnhanceQueryCommand enhances the provided query command with either generated commands based on the provided module
// options or the provided custom commands for each module. If the provided query command already contains a command
// for a module, that command is not over-written by this method. This allows a graceful addition of autocli to
// automatically fill in missing commands.
func (b *Builder) EnhanceQueryCommand(queryCmd *cobra.Command, moduleOptions map[string]*autocliv1.ModuleOptions, customCmds map[string]*cobra.Command) error {
	allModuleNames := map[string]bool{}
	for moduleName := range moduleOptions {
		allModuleNames[moduleName] = true
	}
	for moduleName := range customCmds {
		allModuleNames[moduleName] = true
	}

	for moduleName := range allModuleNames {
		// if we have an existing command skip adding one here
		if existing := findSubCommand(queryCmd, moduleName); existing != nil {
			continue
		}

		// if we have a custom command use that instead of generating one
		if custom := customCmds[moduleName]; custom != nil {
			// custom commands get added lower down
			queryCmd.AddCommand(custom)
			continue
		}

		// check for autocli options
		modOpts := moduleOptions[moduleName]
		if modOpts == nil {
			continue
		}

		queryCmdDesc := modOpts.Query
		if queryCmdDesc != nil {
			cmd, err := b.BuildModuleQueryCommand(moduleName, queryCmdDesc)
			if err != nil {
				return err
			}

			queryCmd.AddCommand(cmd)
		}
	}

	return nil
}

// BuildModuleQueryCommand builds the query command for a single module.
func (b *Builder) BuildModuleQueryCommand(moduleName string, cmdDescriptor *autocliv1.ServiceCommandDescriptor) (*cobra.Command, error) {
	cmd := topLevelCmd(moduleName, fmt.Sprintf("Querying commands for the %s module", moduleName))

	err := b.AddQueryServiceCommands(cmd, cmdDescriptor)

	return cmd, err
}

// AddQueryServiceCommands adds a sub-command to the provided command for each
// method in the specified service and returns the command. This can be used in
// order to add auto-generated commands to an existing command.
func (b *Builder) AddQueryServiceCommands(cmd *cobra.Command, cmdDescriptor *autocliv1.ServiceCommandDescriptor) error {
	for cmdName, subCmdDesc := range cmdDescriptor.SubCommands {
		subCmd := topLevelCmd(cmdName, fmt.Sprintf("Querying commands for the %s service", subCmdDesc.Service))
		err := b.AddQueryServiceCommands(subCmd, subCmdDesc)
		if err != nil {
			return err
		}

		cmd.AddCommand(subCmd)
	}

	// skip empty command descriptors
	if cmdDescriptor.Service == "" {
		return nil
	}

	resolver := b.FileResolver
	if resolver == nil {
		resolver = protoregistry.GlobalFiles
	}
	descriptor, err := resolver.FindDescriptorByName(protoreflect.FullName(cmdDescriptor.Service))
	if err != nil {
		return errors.Errorf("can't find service %s: %v", cmdDescriptor.Service, err)
	}

	service := descriptor.(protoreflect.ServiceDescriptor)
	methods := service.Methods()

	rpcOptMap := map[protoreflect.Name]*autocliv1.RpcCommandOptions{}
	for _, option := range cmdDescriptor.RpcCommandOptions {
		name := protoreflect.Name(option.RpcMethod)
		rpcOptMap[name] = option
		// make sure method exists
		if m := methods.ByName(name); m == nil {
			return fmt.Errorf("rpc method %q not found for service %q", name, service.FullName())
		}
	}

	n := methods.Len()
	for i := 0; i < n; i++ {
		methodDescriptor := methods.Get(i)
		methodOpts := rpcOptMap[methodDescriptor.Name()]
		methodCmd, err := b.BuildQueryMethodCommand(methodDescriptor, methodOpts)
		if err != nil {
			return err
		}

		if methodCmd != nil {
			cmd.AddCommand(methodCmd)
		}
	}

	return nil
}

// BuildQueryMethodCommand creates a gRPC query command for the given service method. This can be used to auto-generate
// just a single command for a single service rpc method.
func (b *Builder) BuildQueryMethodCommand(descriptor protoreflect.MethodDescriptor, options *autocliv1.RpcCommandOptions) (*cobra.Command, error) {
	if options == nil {
		// use the defaults
		options = &autocliv1.RpcCommandOptions{}
	}

	if options.Skip {
		return nil, nil
	}

	serviceDescriptor := descriptor.Parent().(protoreflect.ServiceDescriptor)

	long := options.Long
	if long == "" {
		long = util.DescriptorDocs(descriptor)
	}

	getClientConn := b.GetClientConn
	methodName := fmt.Sprintf("/%s/%s", serviceDescriptor.FullName(), descriptor.Name())

	inputDesc := descriptor.Input()
	inputType := util.ResolveMessageType(b.TypeResolver, inputDesc)
	outputType := util.ResolveMessageType(b.TypeResolver, descriptor.Output())

	use := options.Use
	if use == "" {
		use = protoNameToCliName(descriptor.Name())
	}

	cmd := &cobra.Command{
		Use:        use,
		Long:       long,
		Short:      options.Short,
		Example:    options.Example,
		Aliases:    options.Alias,
		SuggestFor: options.SuggestFor,
		Deprecated: options.Deprecated,
		Version:    options.Version,
	}

	binder, err := b.AddMessageFlags(cmd.Context(), cmd.Flags(), inputType, options)
	if err != nil {
		return nil, err
	}

	cmd.Args = binder.CobraArgs

	jsonMarshalOptions := protojson.MarshalOptions{
		Indent:          "  ",
		UseProtoNames:   true,
		UseEnumNumbers:  false,
		EmitUnpopulated: true,
		Resolver:        b.TypeResolver,
	}

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		clientConn, err := getClientConn(cmd)
		if err != nil {
			return err
		}

		input, err := binder.BuildMessage(args)
		if err != nil {
			return err
		}

		output := outputType.New()
		ctx := cmd.Context()
		err = clientConn.Invoke(ctx, methodName, input.Interface(), output.Interface())
		if err != nil {
			return err
		}

		bz, err := jsonMarshalOptions.Marshal(output.Interface())
		if err != nil {
			return err
		}

		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(bz))
		return err
	}

	if b.AddQueryConnFlags != nil {
		b.AddQueryConnFlags(cmd)
	}

	return cmd, nil
}

func protoNameToCliName(name protoreflect.Name) string {
	return strcase.ToKebab(string(name))
}

func topLevelCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:                        use,
		Short:                      short,
		DisableFlagParsing:         false,
		SuggestionsMinimumDistance: 2,
		RunE:                       validateCmd,
	}
}
