package handlers

import (
	"context"
	"fmt"
	"sort"

	"golang.org/x/exp/constraints"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/runtime/protoiface"

	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	gogogrpc "github.com/cosmos/gogoproto/grpc"
	"github.com/cosmos/gogoproto/proto"

	"github.com/provenance-io/provenance/internal/antewrapper"
	"github.com/provenance-io/provenance/internal/protocompat"
	internalsdk "github.com/provenance-io/provenance/internal/sdk"
	msgfeeskeeper "github.com/provenance-io/provenance/x/msgfees/keeper"
)

// PioMsgServiceRouter routes fully-qualified Msg service methods to their handler with additional fee processing of msgs.
type PioMsgServiceRouter struct {
	interfaceRegistry codectypes.InterfaceRegistry
	routes            map[string]MsgServiceHandler
	hybridHandlers    map[string]protocompat.Handler
	msgFeesKeeper     msgfeeskeeper.Keeper
	decoder           sdk.TxDecoder
	circuitBreaker    baseapp.CircuitBreaker
}

var _ gogogrpc.Server = &PioMsgServiceRouter{}

var _ baseapp.IMsgServiceRouter = &PioMsgServiceRouter{}

// NewPioMsgServiceRouter creates a new PioMsgServiceRouter.
func NewPioMsgServiceRouter(decoder sdk.TxDecoder) *PioMsgServiceRouter {
	return &PioMsgServiceRouter{
		routes:         map[string]MsgServiceHandler{},
		hybridHandlers: map[string]protocompat.Handler{},
		decoder:        decoder,
	}
}

// MsgServiceHandler defines a function type which handles Msg service message.
type MsgServiceHandler = func(ctx sdk.Context, req sdk.Msg) (*sdk.Result, error)

func (msr *PioMsgServiceRouter) SetCircuit(cb baseapp.CircuitBreaker) {
	msr.circuitBreaker = cb
}

// Handler returns the MsgServiceHandler for a given msg or nil if not found.
func (msr *PioMsgServiceRouter) Handler(msg sdk.Msg) MsgServiceHandler {
	return msr.routes[sdk.MsgTypeURL(msg)]
}

// HandlerByTypeURL returns the MsgServiceHandler for a given query route path or nil
// if not found.
func (msr *PioMsgServiceRouter) HandlerByTypeURL(typeURL string) MsgServiceHandler {
	return msr.routes[typeURL]
}

// SetMsgFeesKeeper sets the msg based fee keeper for retrieving msg fees.
func (msr *PioMsgServiceRouter) SetMsgFeesKeeper(msgFeesKeeper msgfeeskeeper.Keeper) {
	msr.msgFeesKeeper = msgFeesKeeper
}

func (msr *PioMsgServiceRouter) registerHybridHandler(sd *grpc.ServiceDesc, method grpc.MethodDesc, handler interface{}) error {
	inputName, err := protocompat.RequestFullNameFromMethodDesc(sd, method)
	if err != nil {
		return err
	}
	cdc := codec.NewProtoCodec(msr.interfaceRegistry)
	hybridHandler, err := protocompat.MakeHybridHandler(cdc, sd, method, handler)
	if err != nil {
		return err
	}
	// if circuit breaker is not nil, then we decorate the hybrid handler with the circuit breaker
	if msr.circuitBreaker == nil {
		msr.hybridHandlers[string(inputName)] = hybridHandler
		return nil
	}
	// decorate the hybrid handler with the circuit breaker
	circuitBreakerHybridHandler := func(ctx context.Context, req, resp protoiface.MessageV1) error {
		messageName := codectypes.MsgTypeURL(req)
		allowed, err := msr.circuitBreaker.IsAllowed(ctx, messageName)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("circuit breaker disallows execution of message %s", messageName)
		}
		return hybridHandler(ctx, req, resp)
	}
	msr.hybridHandlers[string(inputName)] = circuitBreakerHybridHandler
	return nil
}

func (msr *PioMsgServiceRouter) registerMsgServiceHandler(sd *grpc.ServiceDesc, method grpc.MethodDesc, handler interface{}) {
	fqMethod := fmt.Sprintf("/%s/%s", sd.ServiceName, method.MethodName)
	methodHandler := method.Handler

	var requestTypeName string

	// NOTE: This is how we pull the concrete request type for each handler for registering in the InterfaceRegistry.
	// This approach is maybe a bit hacky, but less hacky than reflecting on the handler object itself.
	// We use a no-op interceptor to avoid actually calling into the handler itself.
	_, _ = methodHandler(nil, context.Background(), func(i interface{}) error {
		msg, ok := i.(sdk.Msg)
		if !ok {
			// We panic here because there is no other alternative and the app cannot be initialized correctly
			// this should only happen if there is a problem with code generation in which case the app won't
			// work correctly anyway.
			panic(fmt.Errorf("unable to register service method %s: %T does not implement sdk.Msg", fqMethod, i))
		}

		requestTypeName = sdk.MsgTypeURL(msg)
		return nil
	}, noopInterceptor)

	// Check that the service Msg fully-qualified method name has already
	// been registered (via RegisterInterfaces). If the user registers a
	// service without registering according service Msg type, there might be
	// some unexpected behavior down the road. Since we can't return an error
	// (`Server.RegisterService` interface restriction) we panic (at startup).
	reqType, err := msr.interfaceRegistry.Resolve(requestTypeName)
	if err != nil || reqType == nil {
		panic(
			fmt.Errorf(
				"type_url %s has not been registered yet. "+
					"Before calling RegisterService, you must register all interfaces by calling the `RegisterInterfaces` "+
					"method on module.BasicManager. Each module should call `msgservice.RegisterMsgServiceDesc` inside its "+
					"`RegisterInterfaces` method with the `_Msg_serviceDesc` generated by proto-gen",
				requestTypeName,
			),
		)
	}

	// Check that each service is only registered once. If a service is
	// registered more than once, then we should error. Since we can't
	// return an error (`Server.RegisterService` interface restriction) we
	// panic (at startup).
	_, found := msr.routes[requestTypeName]
	if found {
		panic(
			fmt.Errorf(
				"msg service %s has already been registered. Please make sure to only register each service once. "+
					"This usually means that there are conflicting modules registering the same msg service",
				fqMethod,
			),
		)
	}

	msr.routes[requestTypeName] = func(ctx sdk.Context, req sdk.Msg) (*sdk.Result, error) {
		// provenance specific modification to msg service router that handles x/msgfee distribution
		err := msr.consumeMsgFees(ctx, req)
		if err != nil {
			return nil, err
		}

		// original sdk implementation of msg service router
		ctx = ctx.WithEventManager(sdk.NewEventManager())
		interceptor := func(goCtx context.Context, _ interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			goCtx = context.WithValue(goCtx, sdk.SdkContextKey, ctx)
			return handler(goCtx, req)
		}

		if err = internalsdk.ValidateBasic(req); err != nil {
			return nil, err
		}

		if msr.circuitBreaker != nil {
			msgURL := sdk.MsgTypeURL(req)

			var isAllowed bool
			isAllowed, err = msr.circuitBreaker.IsAllowed(ctx, msgURL)
			if err != nil {
				return nil, err
			}

			if !isAllowed {
				return nil, fmt.Errorf("circuit breaker disables execution of this message: %s", msgURL)
			}
		}

		// Call the method handler from the service description with the handler object.
		// We don't do any decoding here because the decoding was already done.
		res, err := methodHandler(handler, ctx, noopDecoder, interceptor)
		if err != nil {
			return nil, err
		}

		resMsg, ok := res.(proto.Message)
		if !ok {
			return nil, sdkerrors.ErrInvalidType.Wrapf("Expecting proto.Message, got %T", resMsg)
		}

		return sdk.WrapServiceResult(ctx, resMsg, err)
	}
}

// RegisterService implements the gRPC Server.RegisterService method. sd is a gRPC
// service description, handler is an object which implements that gRPC service.
//
// This function PANICs:
//   - if it is called before the service `Msg`s have been registered using
//     RegisterInterfaces,
//   - or if a service is being registered twice.
func (msr *PioMsgServiceRouter) RegisterService(sd *grpc.ServiceDesc, handler interface{}) {
	// Adds a top-level query handler based on the gRPC service name.
	for _, method := range sd.Methods {
		msr.registerMsgServiceHandler(sd, method, handler)
		err := msr.registerHybridHandler(sd, method, handler)
		if err != nil {
			panic(err)
		}
	}
}

func (msr *PioMsgServiceRouter) HybridHandlerByMsgName(msgName string) func(ctx context.Context, req, resp protoiface.MessageV1) error {
	return msr.hybridHandlers[msgName]
}

// SetInterfaceRegistry sets the interface registry for the router.
func (msr *PioMsgServiceRouter) SetInterfaceRegistry(interfaceRegistry codectypes.InterfaceRegistry) {
	msr.interfaceRegistry = interfaceRegistry
}

func noopDecoder(_ interface{}) error { return nil }
func noopInterceptor(_ context.Context, _ interface{}, _ *grpc.UnaryServerInfo, _ grpc.UnaryHandler) (interface{}, error) {
	return nil, nil
}

// consumeMsgFees consumes any message based fees for the provided req.
func (msr *PioMsgServiceRouter) consumeMsgFees(ctx sdk.Context, req sdk.Msg) error {
	feeGasMeter, err := antewrapper.GetFeeGasMeter(ctx)
	if err != nil {
		// The x/gov module calls the message service router for proposal messages that have passed.
		// In such cases, the antehandler is not run, so the gas meter will not be a fee gas meter.
		// But those messages were voted on and have passed, so they should be processed regardless of msg fees.
		// So in here, if there's an error getting the fee gas meter, we skip all this msg fee consumption.
		return nil
	}

	tx, err := msr.decoder(ctx.TxBytes())
	if err != nil {
		panic(fmt.Errorf("error decoding txBytes: %w", err))
	}

	feeTx, err := antewrapper.GetFeeTx(tx)
	if err != nil {
		panic(err)
	}

	feeDist, err := msr.msgFeesKeeper.CalculateAdditionalFeesToBePaid(ctx, req)
	if err != nil {
		return err
	}

	if !feeDist.TotalAdditionalFees.IsZero() {
		if !feeGasMeter.IsSimulate() {
			err = antewrapper.EnsureSufficientFloorAndMsgFees(ctx,
				feeTx.GetFee(), msr.msgFeesKeeper.GetFloorGasPrice(ctx),
				ctx.GasMeter().Limit(), feeGasMeter.FeeConsumed().Add(feeDist.TotalAdditionalFees...))
			if err != nil {
				return err
			}
		}

		msgTypeURL := sdk.MsgTypeURL(req)
		// since AccessMsgFee is not always split 50/50 anymore, this fee can be nil when recipients are specified.
		if feeDist.AdditionalModuleFees != nil {
			feeGasMeter.ConsumeFee(feeDist.AdditionalModuleFees, msgTypeURL, "")
		}
		for _, recipient := range sortedKeys(feeDist.RecipientDistributions) {
			coins := feeDist.RecipientDistributions[recipient]
			feeGasMeter.ConsumeFee(coins, msgTypeURL, recipient)
		}
	}

	return nil
}

// sortedKeys gets the keys of a map, sorts them and returns them as a slice.
func sortedKeys[K constraints.Ordered, V any](m map[K]V) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})
	return keys
}
