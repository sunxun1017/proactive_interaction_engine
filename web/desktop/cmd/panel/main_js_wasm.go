//go:build js && wasm

// Command panel runs the typed desktop control client in a browser WebAssembly
// runtime. It never stores or logs the local bearer token.
package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"syscall/js"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	platformv1 "proactive-interaction-engine/gen/go/proactive/platform/v1"
	"proactive-interaction-engine/gen/go/proactive/platform/v1/platformv1connect"
	"proactive-interaction-engine/web/desktop/presenter"
)

const (
	protocolVersion  = "v1"
	initialSubjectID = "user-1"
	rpcTimeout       = 3 * time.Second
)

type application struct {
	document js.Value
	window   js.Value
	token    string
	client   platformv1connect.DesktopControlServiceClient
	ctx      context.Context
	cancel   context.CancelFunc

	mu          sync.Mutex
	latest      *platformv1.DesktopState
	staticFuncs []js.Func
	renderFuncs []js.Func
	renderMu    sync.Mutex
	workMu      sync.Mutex
	work        sync.WaitGroup
	stopping    bool
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	app, err := newApplication(ctx, cancel)
	if err != nil {
		setStartupFailure(err)
		return
	}
	defer app.release()
	app.bindLifecycle()
	app.bindReject()
	app.spawn(func() { app.run(ctx) })
	<-ctx.Done()
	app.work.Wait()
}

func newApplication(ctx context.Context, cancel context.CancelFunc) (*application, error) {
	window := js.Global().Get("window")
	document := js.Global().Get("document")
	if window.IsUndefined() || document.IsUndefined() {
		return nil, errors.New("browser runtime is unavailable")
	}
	meta := document.Call("querySelector", `meta[name="desktop-token"]`)
	if meta.IsNull() || meta.IsUndefined() {
		return nil, errors.New("desktop token is unavailable")
	}
	token := meta.Call("getAttribute", "content").String()
	origin := window.Get("location").Get("origin").String()
	if token == "" || origin == "" {
		return nil, errors.New("desktop endpoint configuration is unavailable")
	}
	return &application{
		document: document,
		window:   window,
		token:    token,
		client: platformv1connect.NewDesktopControlServiceClient(
			http.DefaultClient,
			origin,
			connect.WithGRPCWeb(),
		),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

func (a *application) run(ctx context.Context) {
	request := connect.NewRequest(&platformv1.GetStateRequest{ProtocolVersion: protocolVersion})
	a.authorize(request.Header())
	requestCtx, cancelRequest := context.WithTimeout(ctx, rpcTimeout)
	response, err := a.client.GetState(requestCtx, request)
	cancelRequest()
	if err != nil {
		a.setStatus("本地服务连接失败")
		return
	}
	if err := a.render(response.Msg.GetState()); err != nil {
		a.setStatus("本地状态无效")
		return
	}

	watchRequest := connect.NewRequest(&platformv1.WatchStateRequest{ProtocolVersion: protocolVersion})
	a.authorize(watchRequest.Header())
	stream, err := a.client.WatchState(ctx, watchRequest)
	if err != nil {
		a.setStatus("实时状态不可用")
		return
	}
	defer stream.Close()
	for stream.Receive() {
		if err := a.render(stream.Msg().GetState()); err != nil {
			a.setStatus("本地状态无效")
			return
		}
	}
	if ctx.Err() == nil {
		a.setStatus("实时连接已断开")
	}
}

func (a *application) render(state *platformv1.DesktopState) error {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	view, err := presenter.Build(state)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.latest = proto.Clone(state).(*platformv1.DesktopState)
	a.mu.Unlock()

	avatar, err := a.element("avatar")
	if err != nil {
		return err
	}
	avatar.Set("className", "avatar "+view.Avatar.CSSClass)
	if err := a.setText("avatar-mode", view.Avatar.Mode); err != nil {
		return err
	}
	if err := a.setText("avatar-text", view.Avatar.Text); err != nil {
		return err
	}
	if err := a.setText("camera-indicator", indicator("摄像头", view.CameraEnabled)); err != nil {
		return err
	}
	if err := a.setText("microphone-indicator", indicator("麦克风", view.MicrophoneEnabled)); err != nil {
		return err
	}
	if err := a.setText("biometric-indicator", indicator("生物识别", view.BiometricsEnabled)); err != nil {
		return err
	}

	permissions, err := a.element("permissions")
	if err != nil {
		return err
	}
	permissions.Call("replaceChildren")
	newFuncs := make([]js.Func, 0, len(view.Permissions))
	for _, permission := range view.Permissions {
		row := a.document.Call("createElement", "label")
		input := a.document.Call("createElement", "input")
		input.Set("type", "checkbox")
		input.Set("checked", permission.Enabled)
		input.Call("setAttribute", "data-permission", permission.ID)
		wire := permission.Wire
		callback := js.FuncOf(func(_ js.Value, args []js.Value) any {
			if len(args) == 0 {
				return nil
			}
			target := args[0].Get("target")
			target.Set("disabled", true)
			a.spawn(func() { a.setPermission(wire, target.Get("checked").Bool()) })
			return nil
		})
		newFuncs = append(newFuncs, callback)
		input.Call("addEventListener", "change", callback)
		label := a.document.Call("createElement", "span")
		label.Set("textContent", permission.Label)
		row.Call("append", input, label)
		permissions.Call("append", row)
	}
	a.replaceRenderFuncs(newFuncs)

	providers, err := a.element("providers")
	if err != nil {
		return err
	}
	providers.Call("replaceChildren")
	for _, provider := range view.Providers {
		row := a.document.Call("createElement", "p")
		row.Set("textContent", provider.ID+"："+provider.State+"（"+provider.Reason+"）")
		providers.Call("append", row)
	}
	a.setStatus("本地服务已连接")
	return nil
}

func (a *application) setPermission(permission platformv1.DesktopPermission, enabled bool) {
	request := connect.NewRequest(&platformv1.SetPermissionRequest{
		ProtocolVersion: protocolVersion,
		Permission:      permission,
		Enabled:         enabled,
	})
	a.authorize(request.Header())
	requestCtx, cancelRequest := context.WithTimeout(a.ctx, rpcTimeout)
	response, err := a.client.SetPermission(requestCtx, request)
	cancelRequest()
	if err != nil {
		a.setStatus("权限修改失败")
		a.renderLatest()
		return
	}
	if err := a.render(response.Msg.GetState()); err != nil {
		a.setStatus("权限状态无效")
	}
}

func (a *application) bindReject() {
	button, err := a.element("reject-button")
	if err != nil {
		return
	}
	callback := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		button.Set("disabled", true)
		a.spawn(func() {
			defer button.Set("disabled", false)
			requestID, err := a.randomID()
			if err != nil {
				a.setStatus("无法生成本地请求标识")
				return
			}
			traceID, err := a.randomID()
			if err != nil {
				a.setStatus("无法生成本地追踪标识")
				return
			}
			request := connect.NewRequest(&platformv1.RejectInteractionRequest{
				ProtocolVersion: protocolVersion,
				RequestId:       requestID,
				SubjectId:       initialSubjectID,
				TraceId:         traceID,
			})
			a.authorize(request.Header())
			requestCtx, cancelRequest := context.WithTimeout(a.ctx, rpcTimeout)
			response, err := a.client.RejectInteraction(requestCtx, request)
			cancelRequest()
			if err != nil || !response.Msg.GetAccepted() {
				a.setStatus("停止请求失败")
				return
			}
			a.setStatus("已停止当前互动")
		})
		return nil
	})
	button.Call("addEventListener", "click", callback)
	a.staticFuncs = append(a.staticFuncs, callback)
}

func (a *application) bindLifecycle() {
	callback := js.FuncOf(func(_ js.Value, _ []js.Value) any {
		a.stop()
		return nil
	})
	a.window.Call("addEventListener", "pagehide", callback)
	a.staticFuncs = append(a.staticFuncs, callback)
}

func (a *application) renderLatest() {
	a.mu.Lock()
	latest := a.latest
	if latest != nil {
		latest = proto.Clone(latest).(*platformv1.DesktopState)
	}
	a.mu.Unlock()
	if latest != nil {
		_ = a.render(latest)
	}
}

func (a *application) authorize(header http.Header) {
	header.Set("authorization", "Bearer "+a.token)
}

func (a *application) randomID() (string, error) {
	crypto := a.window.Get("crypto")
	if crypto.IsUndefined() || crypto.IsNull() || crypto.Get("randomUUID").Type() != js.TypeFunction {
		return "", errors.New("browser cryptography is unavailable")
	}
	return crypto.Call("randomUUID").String(), nil
}

func (a *application) element(id string) (js.Value, error) {
	element := a.document.Call("getElementById", id)
	if element.IsNull() || element.IsUndefined() {
		return js.Undefined(), errors.New("desktop element is unavailable")
	}
	return element, nil
}

func (a *application) setText(id, value string) error {
	element, err := a.element(id)
	if err != nil {
		return err
	}
	element.Set("textContent", value)
	return nil
}

func (a *application) setStatus(value string) {
	_ = a.setText("connection-status", value)
}

func (a *application) replaceRenderFuncs(next []js.Func) {
	previous := a.renderFuncs
	a.renderFuncs = next
	for _, callback := range previous {
		callback.Release()
	}
}

func (a *application) release() {
	for _, callback := range a.renderFuncs {
		callback.Release()
	}
	for _, callback := range a.staticFuncs {
		callback.Release()
	}
}

func (a *application) spawn(work func()) {
	a.workMu.Lock()
	if a.stopping {
		a.workMu.Unlock()
		return
	}
	a.work.Add(1)
	a.workMu.Unlock()
	go func() {
		defer a.work.Done()
		work()
	}()
}

func (a *application) stop() {
	a.workMu.Lock()
	if !a.stopping {
		a.stopping = true
		a.cancel()
	}
	a.workMu.Unlock()
}

func indicator(label string, enabled bool) string {
	state := "未启用"
	if enabled {
		state = "已启用"
	}
	return label + "：" + state
}

func setStartupFailure(_ error) {
	document := js.Global().Get("document")
	if document.IsUndefined() {
		return
	}
	status := document.Call("getElementById", "connection-status")
	if !status.IsNull() && !status.IsUndefined() {
		status.Set("textContent", "本地面板启动失败")
	}
}
