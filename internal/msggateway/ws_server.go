package msggateway

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openimsdk/open-im-server/v3/pkg/common/webhook"
	"github.com/openimsdk/open-im-server/v3/pkg/rpccache"
	pbAuth "github.com/openimsdk/protocol/auth"
	"github.com/openimsdk/tools/mcontext"

	"github.com/go-playground/validator/v10"
	"github.com/openimsdk/open-im-server/v3/pkg/common/prommetrics"
	"github.com/openimsdk/open-im-server/v3/pkg/common/servererrs"
	"github.com/openimsdk/open-im-server/v3/pkg/rpcclient"
	"github.com/openimsdk/protocol/constant"
	"github.com/openimsdk/protocol/msggateway"
	"github.com/openimsdk/tools/discovery"
	"github.com/openimsdk/tools/errs"
	"github.com/openimsdk/tools/log"
	"github.com/openimsdk/tools/utils/stringutil"
	"golang.org/x/sync/errgroup"
)

type LongConnServer interface {
	Run(done chan error) error
	wsHandler(w http.ResponseWriter, r *http.Request)
	GetUserAllCons(userID string) ([]*Client, bool)
	GetUserPlatformCons(userID string, platform int) ([]*Client, bool, bool)
	Validate(s any) error
	SetDiscoveryRegistry(client discovery.SvcDiscoveryRegistry, config *Config)
	KickUserConn(client *Client) error
	UnRegister(c *Client)
	SetKickHandlerInfo(i *kickHandler)
	SubUserOnlineStatus(ctx context.Context, client *Client, data *Req) ([]byte, error)
	Compressor
	MessageHandler
}

type WsServer struct {
	msgGatewayConfig  *Config
	port              int
	wsMaxConnNum      int64
	registerChan      chan *Client
	unregisterChan    chan *Client
	kickHandlerChan   chan *kickHandler
	clients           UserMap
	online            *rpccache.OnlineCache
	subscription      *Subscription
	clientPool        sync.Pool
	onlineUserNum     atomic.Int64
	onlineUserConnNum atomic.Int64
	handshakeTimeout  time.Duration
	writeBufferSize   int
	validate          *validator.Validate
	userClient        *rpcclient.UserRpcClient
	authClient        *rpcclient.Auth
	disCov            discovery.SvcDiscoveryRegistry
	Compressor
	//Encoder
	MessageHandler
	webhookClient *webhook.Client
}

type kickHandler struct {
	clientOK   bool
	oldClients []*Client
	newClient  *Client
}

func (ws *WsServer) SetDiscoveryRegistry(disCov discovery.SvcDiscoveryRegistry, config *Config) {
	ws.MessageHandler = NewGrpcHandler(ws.validate, disCov, &config.Share.RpcRegisterName)
	u := rpcclient.NewUserRpcClient(disCov, config.Share.RpcRegisterName.User, config.Share.IMAdminUserID)
	ws.authClient = rpcclient.NewAuth(disCov, config.Share.RpcRegisterName.Auth)
	ws.userClient = &u
	ws.disCov = disCov
}

//func (ws *WsServer) SetUserOnlineStatus(ctx context.Context, client *Client, status int32) {
//	err := ws.userClient.SetUserStatus(ctx, client.UserID, status, client.PlatformID)
//	if err != nil {
//		log.ZWarn(ctx, "SetUserStatus err", err)
//	}
//	switch status {
//	case constant.Online:
//		ws.webhookAfterUserOnline(ctx, &ws.msgGatewayConfig.WebhooksConfig.AfterUserOnline, client.UserID, client.PlatformID, client.IsBackground, client.ctx.GetConnID())
//	case constant.Offline:
//		ws.webhookAfterUserOffline(ctx, &ws.msgGatewayConfig.WebhooksConfig.AfterUserOffline, client.UserID, client.PlatformID, client.ctx.GetConnID())
//	}
//}

func (ws *WsServer) UnRegister(c *Client) {
	ws.unregisterChan <- c
}

func (ws *WsServer) Validate(_ any) error {
	return nil
}

func (ws *WsServer) GetUserAllCons(userID string) ([]*Client, bool) {
	return ws.clients.GetAll(userID)
}

func (ws *WsServer) GetUserPlatformCons(userID string, platform int) ([]*Client, bool, bool) {
	return ws.clients.Get(userID, platform)
}

func NewWsServer(msgGatewayConfig *Config, opts ...Option) *WsServer {
	var config configs
	for _, o := range opts {
		o(&config)
	}
	//userRpcClient := rpcclient.NewUserRpcClient(client, config.Share.RpcRegisterName.User, config.Share.IMAdminUserID)

	v := validator.New()
	return &WsServer{
		msgGatewayConfig: msgGatewayConfig,
		port:             config.port,
		wsMaxConnNum:     config.maxConnNum,
		writeBufferSize:  config.writeBufferSize,
		handshakeTimeout: config.handshakeTimeout,
		clientPool: sync.Pool{
			New: func() any {
				return new(Client)
			},
		},
		registerChan:    make(chan *Client, 1000),
		unregisterChan:  make(chan *Client, 1000),
		kickHandlerChan: make(chan *kickHandler, 1000),
		validate:        v,
		clients:         newUserMap(),
		subscription:    newSubscription(),
		Compressor:      NewGzipCompressor(),
		webhookClient:   webhook.NewWebhookClient(msgGatewayConfig.WebhooksConfig.URL),
	}
}

func (ws *WsServer) Run(done chan error) error {
	var (
		client       *Client
		netErr       error
		shutdownDone = make(chan struct{}, 1)
	)

	server := http.Server{Addr: ":" + stringutil.IntToString(ws.port), Handler: nil}

	go func() {
		for {
			select {
			case <-shutdownDone:
				return
			case client = <-ws.registerChan:
				ws.registerClient(client)
			case client = <-ws.unregisterChan:
				ws.unregisterClient(client)
			case onlineInfo := <-ws.kickHandlerChan:
				ws.multiTerminalLoginChecker(onlineInfo.clientOK, onlineInfo.oldClients, onlineInfo.newClient)
			}
		}
	}()
	netDone := make(chan struct{}, 1)
	go func() {
		http.HandleFunc("/", ws.wsHandler)
		err := server.ListenAndServe()
		defer close(netDone)
		if err != nil && err != http.ErrServerClosed {
			netErr = errs.WrapMsg(err, "ws start err", server.Addr)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var err error
	select {
	case err = <-done:
		sErr := server.Shutdown(ctx)
		if sErr != nil {
			return errs.WrapMsg(sErr, "shutdown err")
		}
		close(shutdownDone)
		if err != nil {
			return err
		}
	case <-netDone:
	}
	return netErr

}

var concurrentRequest = 3

func (ws *WsServer) sendUserOnlineInfoToOtherNode(ctx context.Context, client *Client) error {
	conns, err := ws.disCov.GetConns(ctx, ws.msgGatewayConfig.Share.RpcRegisterName.MessageGateway)
	if err != nil {
		return err
	}
	wg := errgroup.Group{}
	wg.SetLimit(concurrentRequest)

	// Online push user online message to other node
	for _, v := range conns {
		v := v
		log.ZDebug(ctx, " sendUserOnlineInfoToOtherNode conn ", "target", v.Target())
		if v.Target() == ws.disCov.GetSelfConnTarget() {
			log.ZDebug(ctx, "Filter out this node", "node", v.Target())
			continue
		}

		wg.Go(func() error {
			msgClient := msggateway.NewMsgGatewayClient(v)
			_, err := msgClient.MultiTerminalLoginCheck(ctx, &msggateway.MultiTerminalLoginCheckReq{
				UserID:     client.UserID,
				PlatformID: int32(client.PlatformID), Token: client.token,
			})
			if err != nil {
				log.ZWarn(ctx, "MultiTerminalLoginCheck err", err, "node", v.Target())
			}
			return nil
		})
	}

	_ = wg.Wait()
	return nil
}

func (ws *WsServer) SetKickHandlerInfo(i *kickHandler) {
	ws.kickHandlerChan <- i
}

func (ws *WsServer) registerClient(client *Client) {
	var (
		userOK     bool
		clientOK   bool
		oldClients []*Client
	)
	oldClients, userOK, clientOK = ws.clients.Get(client.UserID, client.PlatformID)
	if !userOK {
		ws.clients.Set(client.UserID, client)
		log.ZDebug(client.ctx, "user not exist", "userID", client.UserID, "platformID", client.PlatformID)
		prommetrics.OnlineUserGauge.Add(1)
		ws.onlineUserNum.Add(1)
		ws.onlineUserConnNum.Add(1)
	} else {
		ws.multiTerminalLoginChecker(clientOK, oldClients, client)
		log.ZDebug(client.ctx, "user exist", "userID", client.UserID, "platformID", client.PlatformID)
		if clientOK {
			ws.clients.Set(client.UserID, client)
			// There is already a connection to the platform
			log.ZDebug(client.ctx, "repeat login", "userID", client.UserID, "platformID",
				client.PlatformID, "old remote addr", getRemoteAdders(oldClients))
			ws.onlineUserConnNum.Add(1)
		} else {
			ws.clients.Set(client.UserID, client)
			ws.onlineUserConnNum.Add(1)
		}
	}

	wg := sync.WaitGroup{}
	log.ZDebug(client.ctx, "ws.msgGatewayConfig.Discovery.Enable", "discoveryEnable", ws.msgGatewayConfig.Discovery.Enable)

	if ws.msgGatewayConfig.Discovery.Enable != "k8s" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ws.sendUserOnlineInfoToOtherNode(client.ctx, client)
		}()
	}

	//wg.Add(1)
	//go func() {
	//	defer wg.Done()
	//	ws.SetUserOnlineStatus(client.ctx, client, constant.Online)
	//}()

	wg.Wait()

	log.ZDebug(client.ctx, "user online", "online user Num", ws.onlineUserNum.Load(), "online user conn Num", ws.onlineUserConnNum.Load())
}

func getRemoteAdders(client []*Client) string {
	var ret string
	for i, c := range client {
		if i == 0 {
			ret = c.ctx.GetRemoteAddr()
		} else {
			ret += "@" + c.ctx.GetRemoteAddr()
		}
	}
	return ret
}

func (ws *WsServer) KickUserConn(client *Client) error {
	ws.clients.DeleteClients(client.UserID, []*Client{client})
	return client.KickOnlineMessage()
}

func (ws *WsServer) multiTerminalLoginChecker(clientOK bool, oldClients []*Client, newClient *Client) {
	kickTokenFunc := func(kickClients []*Client) {
		var kickTokens []string
		ws.clients.DeleteClients(newClient.UserID, kickClients)
		for _, c := range kickClients {
			kickTokens = append(kickTokens, c.token)
			err := c.KickOnlineMessage()
			if err != nil {
				log.ZWarn(c.ctx, "KickOnlineMessage", err)
			}
		}
		ctx := mcontext.WithMustInfoCtx(
			[]string{newClient.ctx.GetOperationID(), newClient.ctx.GetUserID(),
				constant.PlatformIDToName(newClient.PlatformID), newClient.ctx.GetConnID()},
		)
		if _, err := ws.authClient.KickTokens(ctx, kickTokens); err != nil {
			log.ZWarn(newClient.ctx, "kickTokens err", err)
		}
	}

	switch ws.msgGatewayConfig.Share.MultiLogin.Policy {
	case constant.DefalutNotKick:
	case constant.PCAndOther:
		if constant.PlatformIDToClass(newClient.PlatformID) == constant.TerminalPC {
			return
		}
		fallthrough
	case constant.AllLoginButSameTermKick:
		if !clientOK {
			return
		}
		ws.clients.DeleteClients(newClient.UserID, oldClients)
		for _, c := range oldClients {
			err := c.KickOnlineMessage()
			if err != nil {
				log.ZWarn(c.ctx, "KickOnlineMessage", err)
			}
		}
		ctx := mcontext.WithMustInfoCtx(
			[]string{newClient.ctx.GetOperationID(), newClient.ctx.GetUserID(),
				constant.PlatformIDToName(newClient.PlatformID), newClient.ctx.GetConnID()},
		)
		if _, err := ws.authClient.InvalidateToken(ctx, newClient.token, newClient.UserID, newClient.PlatformID); err != nil {
			log.ZWarn(newClient.ctx, "InvalidateToken err", err, "userID", newClient.UserID,
				"platformID", newClient.PlatformID)
		}
	case constant.AllLoginButSameClassKick:
		clients, ok := ws.clients.GetAll(newClient.UserID)
		if !ok {
			return
		}
		var (
			kickClients []*Client
		)
		for _, client := range clients {
			if constant.PlatformIDToClass(client.PlatformID) == constant.PlatformIDToClass(newClient.PlatformID) {
				kickClients = append(kickClients, client)
			}
		}
		kickTokenFunc(kickClients)
	}
}

func (ws *WsServer) unregisterClient(client *Client) {
	defer ws.clientPool.Put(client)
	isDeleteUser := ws.clients.DeleteClients(client.UserID, []*Client{client})
	if isDeleteUser {
		ws.onlineUserNum.Add(-1)
		prommetrics.OnlineUserGauge.Dec()
	}
	ws.onlineUserConnNum.Add(-1)
	ws.subscription.DelClient(client)
	//ws.SetUserOnlineStatus(client.ctx, client, constant.Offline)
	log.ZDebug(client.ctx, "user offline", "close reason", client.closedErr, "online user Num",
		ws.onlineUserNum.Load(), "online user conn Num",
		ws.onlineUserConnNum.Load(),
	)
}

// validateRespWithRequest checks if the response matches the expected userID and platformID.
func (ws *WsServer) validateRespWithRequest(ctx *UserConnContext, resp *pbAuth.ParseTokenResp) error {
	userID := ctx.GetUserID()
	platformID := stringutil.StringToInt32(ctx.GetPlatformID())
	if resp.UserID != userID {
		return servererrs.ErrTokenInvalid.WrapMsg(fmt.Sprintf("token uid %s != userID %s", resp.UserID, userID))
	}
	if resp.PlatformID != platformID {
		return servererrs.ErrTokenInvalid.WrapMsg(fmt.Sprintf("token platform %d != platformID %d", resp.PlatformID, platformID))
	}
	return nil
}

func (ws *WsServer) wsHandler(w http.ResponseWriter, r *http.Request) {
	// Create a new connection context
	connContext := newContext(w, r)

	// Check if the current number of online user connections exceeds the maximum limit
	if ws.onlineUserConnNum.Load() >= ws.wsMaxConnNum {
		// If it exceeds the maximum connection number, return an error via HTTP and stop processing
		httpError(connContext, servererrs.ErrConnOverMaxNumLimit.WrapMsg("over max conn num limit"))
		return
	}

	// Parse essential arguments (e.g., user ID, Token)
	err := connContext.ParseEssentialArgs()
	if err != nil {
		// If there's an error during parsing, return an error via HTTP and stop processing

		httpError(connContext, err)
		return
	}

	// Call the authentication client to parse the Token obtained from the context
	resp, err := ws.authClient.ParseToken(connContext, connContext.GetToken())
	if err != nil {
		// If there's an error parsing the Token, decide whether to send the error message via WebSocket based on the context flag
		shouldSendError := connContext.ShouldSendResp()
		if shouldSendError {
			// Create a WebSocket connection object and attempt to send the error message via WebSocket
			wsLongConn := newGWebSocket(WebSocket, ws.handshakeTimeout, ws.writeBufferSize)
			if err := wsLongConn.RespondWithError(err, w, r); err == nil {
				// If the error message is successfully sent via WebSocket, stop processing
				return
			}
		}
		// If sending via WebSocket is not required or fails, return the error via HTTP and stop processing
		httpError(connContext, err)
		return
	}

	// Validate the authentication response matches the request (e.g., user ID and platform ID)
	err = ws.validateRespWithRequest(connContext, resp)
	if err != nil {
		// If validation fails, return an error via HTTP and stop processing
		httpError(connContext, err)
		return
	}

	log.ZDebug(connContext, "new conn", "token", connContext.GetToken())
	// Create a WebSocket long connection object
	wsLongConn := newGWebSocket(WebSocket, ws.handshakeTimeout, ws.writeBufferSize)
	if err := wsLongConn.GenerateLongConn(w, r); err != nil {
		//If the creation of the long connection fails, the error is handled internally during the handshake process.
		log.ZWarn(connContext, "long connection fails", err)
		return
	} else {
		// Check if a normal response should be sent via WebSocket
		shouldSendSuccessResp := connContext.ShouldSendResp()
		if shouldSendSuccessResp {
			// Attempt to send a success message through WebSocket
			if err := wsLongConn.RespondWithSuccess(); err != nil {
				// If the success message is successfully sent, end further processing
				return
			}
		}
	}

	// Retrieve a client object from the client pool, reset its state, and associate it with the current WebSocket long connection
	client := ws.clientPool.Get().(*Client)
	client.ResetClient(connContext, wsLongConn, ws)

	// Register the client with the server and start message processing
	ws.registerChan <- client
	go client.readMessage()
}
