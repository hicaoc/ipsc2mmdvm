package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/USA-RedDragon/configulator"
	"github.com/hicaoc/ipsc2mmdvm/internal/audio"
	"github.com/hicaoc/ipsc2mmdvm/internal/config"
	"github.com/hicaoc/ipsc2mmdvm/internal/dmrid"
	"github.com/hicaoc/ipsc2mmdvm/internal/hytera"
	"github.com/hicaoc/ipsc2mmdvm/internal/ipsc"
	"github.com/hicaoc/ipsc2mmdvm/internal/metrics"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm"
	"github.com/hicaoc/ipsc2mmdvm/internal/mmdvm/proto"
	"github.com/hicaoc/ipsc2mmdvm/internal/registry"
	"github.com/hicaoc/ipsc2mmdvm/internal/repeater"
	"github.com/hicaoc/ipsc2mmdvm/internal/routing"
	"github.com/hicaoc/ipsc2mmdvm/internal/timeslot"
	webui "github.com/hicaoc/ipsc2mmdvm/internal/web"
	"github.com/lmittmann/tint"
	"github.com/spf13/cobra"
	"github.com/ztrue/shutdown"
)

func NewCommand(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ipsc2mmdvm",
		Version: fmt.Sprintf("%s - %s", version, commit),
		Annotations: map[string]string{
			"version": version,
			"commit":  commit,
		},
		RunE:              runRoot,
		SilenceErrors:     true,
		DisableAutoGenTag: true,
	}
	return cmd
}

func runRoot(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	fmt.Printf("ipsc2mmdvm - %s (%s)\n", cmd.Annotations["version"], cmd.Annotations["commit"])

	c, err := configulator.FromContext[config.Config](ctx)
	if err != nil {
		return fmt.Errorf("failed to get config from context")
	}

	cfg, err := c.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	var logger *slog.Logger
	switch cfg.LogLevel {
	case config.LogLevelDebug:
		logger = slog.New(tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelDebug}))
	case config.LogLevelInfo:
		logger = slog.New(tint.NewHandler(os.Stdout, &tint.Options{Level: slog.LevelInfo}))
	case config.LogLevelWarn:
		logger = slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelWarn}))
	case config.LogLevelError:
		logger = slog.New(tint.NewHandler(os.Stderr, &tint.Options{Level: slog.LevelError}))
	}
	slog.SetDefault(logger)

	// Create metrics and optionally start the metrics HTTP server.
	var m *metrics.Metrics
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled && cfg.Metrics.Address != "" {
		m = metrics.NewMetrics()
		mux := http.NewServeMux()
		mux.Handle("/metrics", m.Handler())
		metricsSrv = &http.Server{
			Addr:    cfg.Metrics.Address,
			Handler: mux,
		}
		go func() {
			slog.Info("Starting metrics server", "address", cfg.Metrics.Address)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Metrics server error", "error", err)
			}
		}()
	}

	resolver, err := loadDMRIDResolver(cfg)
	if err != nil {
		return fmt.Errorf("failed to load dmrid database: %w", err)
	}

	store, err := registry.Open(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("failed to open sqlite store: %w", err)
	}
	registrySvc, err := registry.NewService(store, resolver)
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("failed to initialize registry service: %w", err)
	}
	forwardAllowed := func(targetKey string) bool {
		if targetKey == "" {
			return false
		}
		dev, ok := registrySvc.FindDevice(targetKey)
		if !ok {
			return true
		}
		return !dev.Disabled
	}
	var handleIngress func(frontend, source string, pkt proto.Packet, addr *net.UDPAddr)
	audioHub := audio.NewHub()
	dmrAudioPool, err := audio.NewDMRDecoderPool(audioHub, 4)
	if err != nil {
		return fmt.Errorf("failed to initialize DMR audio decoder pool: %w", err)
	}
	defer func() {
		_ = dmrAudioPool.Close()
	}()

	// Create one MMDVM client per configured network (DMR master).
	// All clients share a single outbound timeslot manager so that
	// only one master can feed a given timeslot toward IPSC at a time.
	outboundTSMgr := timeslot.NewManager()
	if m != nil {
		outboundTSMgr.SetMetrics(m, "outbound")
	}
	totalMMDVMNetworks := len(cfg.MMDVMClients) + len(cfg.MMDVMServers)
	mmdvmNetworks := make([]mmdvm.Network, 0, totalMMDVMNetworks)
	mmdvmUpstreams := map[string]mmdvm.Network{}
	var mmdvmServers []*mmdvm.MMDVMServer
	for i := range cfg.MMDVMClients {
		network := mmdvm.NewMMDVMClient(&cfg.MMDVMClients[i], m)
		mmdvmUpstreams["mmdvm-upstream:"+cfg.MMDVMClients[i].Name] = network
		network.SetStatusHandler(func(status mmdvm.ClientStatus) {
			host := status.Master
			port := 0
			if parsedHost, parsedPort, err := net.SplitHostPort(status.Master); err == nil {
				host = parsedHost
				if p, err := strconv.Atoi(parsedPort); err == nil {
					port = p
				}
			}
			_, err := registrySvc.UpsertDevice(registry.Device{
				Category:    registry.CategoryMMDVM,
				Protocol:    "mmdvm-upstream",
				SourceKey:   status.SourceKey,
				Name:        status.Name,
				Callsign:    status.Callsign,
				DMRID:       status.DMRID,
				Model:       "MMDVM Master",
				IP:          host,
				Port:        port,
				Status:      status.Status,
				Online:      status.Online,
				LastSeenAt:  status.LastSeenAt,
				RXFreq:      status.RXFreq,
				TXFreq:      status.TXFreq,
				TXPower:     status.TXPower,
				ColorCode:   status.ColorCode,
				Latitude:    status.Latitude,
				Longitude:   status.Longitude,
				Height:      status.Height,
				Location:    status.Location,
				Description: status.Description,
				URL:         status.URL,
				Slots:       status.Slots,
			})
			if err != nil {
				slog.Warn("failed to persist MMDVM upstream status", "network", status.Name, "error", err)
			}
		})
		network.SetOutboundTSManager(outboundTSMgr)
		err = network.Start()
		if err != nil {
			return fmt.Errorf("failed to start MMDVM client %q: %w", cfg.MMDVMClients[i].Name, err)
		}
		mmdvmNetworks = append(mmdvmNetworks, network)
	}
	for i := range cfg.MMDVMServers {
		server := mmdvm.NewMMDVMServer(&cfg.MMDVMServers[i], m)
		server.SetSendFilter(forwardAllowed)
		server.SetPacketSourceHandler(func(sourceKey string, packet proto.Packet) {
			handleIngress("mmdvm", sourceKey, packet, nil)
		})
		mmdvmServers = append(mmdvmServers, server)
		server.SetPeerUpdateHandler(func(peer mmdvm.PeerInfo) {
			status := "online"
			if !peer.Online {
				status = "offline"
			}
			stored, err := registrySvc.UpsertDevice(registry.Device{
				Category:    registry.CategoryMMDVM,
				Protocol:    "mmdvm",
				SourceKey:   peer.SourceKey,
				Name:        peer.Listener,
				Callsign:    peer.Callsign,
				DMRID:       peer.DMRID,
				Model:       peer.Model,
				IP:          peer.IP,
				Port:        peer.Port,
				Status:      status,
				Online:      peer.Online,
				LastSeenAt:  peer.LastSeenAt,
				RXFreq:      peer.RXFreq,
				TXFreq:      peer.TXFreq,
				TXPower:     peer.TXPower,
				ColorCode:   peer.ColorCode,
				Latitude:    peer.Latitude,
				Longitude:   peer.Longitude,
				Height:      peer.Height,
				Location:    peer.Location,
				Description: peer.Description,
				URL:         peer.URL,
				Slots:       peer.Slots,
			})
			if err != nil {
				slog.Warn("failed to persist MMDVM peer", "peer", peer.SourceKey, "error", err)
				return
			}
			slog.Debug("persisted MMDVM peer",
				"sourceKey", stored.SourceKey,
				"dmrid", stored.DMRID,
				"callsign", stored.Callsign,
				"online", stored.Online,
				"ip", stored.IP,
				"port", stored.Port)
		})
		server.SetOutboundTSManager(outboundTSMgr)
		err = server.Start()
		if err != nil {
			return fmt.Errorf("failed to start MMDVM server %q: %w", cfg.MMDVMServers[i].Name, err)
		}
		mmdvmNetworks = append(mmdvmNetworks, server)
	}

	var (
		ipscServer   *ipsc.IPSCServer
		hyteraServer *hytera.Server
	)
	ingressGuard := repeater.NewGuard()
	groupRouter := routing.NewSubscriptionManager(15 * time.Minute)
	privateRouter := newRecentPrivateRouteCache(recentPrivateRouteTTL)
	staticGroups, err := registrySvc.LoadStaticGroups()
	if err != nil {
		return fmt.Errorf("failed to load static group subscriptions: %w", err)
	}
	staticByDevice := map[string]map[routing.Slot][]uint32{}
	for _, group := range staticGroups {
		slot := routing.Slot(group.Slot)
		if slot != routing.Slot1 && slot != routing.Slot2 {
			continue
		}
		if staticByDevice[group.SourceKey] == nil {
			staticByDevice[group.SourceKey] = map[routing.Slot][]uint32{}
		}
		staticByDevice[group.SourceKey][slot] = append(staticByDevice[group.SourceKey][slot], group.GroupID)
	}
	for sourceKey, slots := range staticByDevice {
		for slot, groups := range slots {
			groupRouter.ReplaceStatic(sourceKey, slot, groups)
		}
	}

	admin := cfg.Web.BootstrapAdmin
	adminCreatedFromDefaults := false
	if admin.Username == "" || admin.Password == "" || admin.Callsign == "" || admin.Email == "" {
		admin = config.WebBootstrapAdmin{
			Username: "admin",
			Callsign: "ADMIN",
			Email:    "admin@localhost",
			Password: "admin123456",
		}
		adminCreatedFromDefaults = true
	}
	hash, err := webui.HashPassword(admin.Password)
	if err != nil {
		return fmt.Errorf("failed to hash bootstrap admin password: %w", err)
	}
	_, createdAdmin, err := registrySvc.EnsureAdminUser(registry.User{
		Username:     admin.Username,
		Callsign:     strings.ToUpper(admin.Callsign),
		Email:        admin.Email,
		PasswordHash: hash,
	})
	if err != nil {
		return fmt.Errorf("failed to ensure bootstrap admin user: %w", err)
	}
	if adminCreatedFromDefaults && createdAdmin {
		slog.Warn("Created default bootstrap admin user; change the password immediately",
			"username", admin.Username,
			"password", admin.Password,
			"email", admin.Email)
	}

	var webSrv *http.Server
	if cfg.Web.Enabled && cfg.Web.Address != "" {
		webSrv = &http.Server{
			Addr:    cfg.Web.Address,
			Handler: webui.NewServer(registrySvc, groupRouter, audioHub, cfg).Handler(),
		}
		go func() {
			slog.Info("Starting web management server", "address", cfg.Web.Address)
			if err := webSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("Web server error", "error", err)
			}
		}()
	}

	var (
		motoTranslatorMu sync.Mutex
		motoTranslators  = map[string]*ipsc.IPSCTranslator{}
	)
	var (
		routeMotoTranslatorMu sync.Mutex
		routeMotoTranslators  = map[string]*ipsc.IPSCTranslator{}
	)
	getMotoTranslator := func(source string) *ipsc.IPSCTranslator {
		motoTranslatorMu.Lock()
		defer motoTranslatorMu.Unlock()
		tr := motoTranslators[source]
		if tr != nil {
			return tr
		}
		var err error
		tr, err = ipsc.NewIPSCTranslator()
		if err != nil {
			slog.Warn("failed creating IPSC translator", "source", source, "error", err)
			return nil
		}
		motoTranslators[source] = tr
		return tr
	}
	getRouteMotoTranslator := func(sourceDeviceKey, targetKey string) *ipsc.IPSCTranslator {
		if sourceDeviceKey == "" || targetKey == "" {
			return nil
		}
		key := sourceDeviceKey + "->" + targetKey
		routeMotoTranslatorMu.Lock()
		defer routeMotoTranslatorMu.Unlock()
		tr := routeMotoTranslators[key]
		if tr != nil {
			return tr
		}
		var err error
		tr, err = ipsc.NewIPSCTranslator()
		if err != nil {
			slog.Warn("failed creating routed IPSC translator", "route", key, "error", err)
			return nil
		}
		tr.SetPeerID(cfg.BridgeID())
		routeMotoTranslators[key] = tr
		return tr
	}

	dispatchToMMDVM := func(pkt proto.Packet, addr *net.UDPAddr) bool {
		for _, network := range mmdvmNetworks {
			if _, isServer := network.(*mmdvm.MMDVMServer); !isServer && !forwardAllowed("mmdvm-upstream:"+network.Name()) {
				continue
			}
			if network.MatchesPacket(pkt, false) {
				return network.HandleTranslatedPacket(pkt, addr)
			}
		}
		for _, network := range mmdvmNetworks {
			if _, isServer := network.(*mmdvm.MMDVMServer); !isServer && !forwardAllowed("mmdvm-upstream:"+network.Name()) {
				continue
			}
			if network.MatchesPacket(pkt, true) {
				return network.HandleTranslatedPacket(pkt, addr)
			}
		}
		return false
	}
	slotFromPacket := func(pkt proto.Packet) routing.Slot {
		if pkt.Slot {
			return routing.Slot2
		}
		return routing.Slot1
	}
	recordCall := func(frontend string, sourceKey string, category registry.DeviceCategory, pkt proto.Packet, addr *net.UDPAddr) {
		slot := 1
		if pkt.Slot {
			slot = 2
		}
		callType := "group"
		if !pkt.GroupCall {
			callType = "private"
		}
		isTerminator := pkt.FrameType == 2 && pkt.DTypeOrVSeq == 2
		device, _ := registrySvc.FindDevice(sourceKey)
		sourceDeviceCallsign := strings.TrimSpace(device.Callsign)
		sourceName := sourceDeviceCallsign
		if sourceName == "" {
			sourceName = strings.TrimSpace(device.Name)
		}
		if sourceName == "" {
			sourceName = sourceKey
		}
		call := registry.CallRecord{
			Frontend:       frontend,
			SourceCategory: category,
			SourceKey:      sourceKey,
			SourceName:     sourceName,
			// Keep source callsign as caller identity (resolved by source DMRID).
			// Device-side callsign is carried via SourceName.
			SourceCallsign: "",
			SourceDMRID:    uint32(pkt.Src),
			SrcID:          pkt.Src,
			DstID:          pkt.Dst,
			RepeaterID:     pkt.Repeater,
			Slot:           slot,
			CallType:       callType,
			StreamID:       pkt.StreamID,
		}
		if addr != nil {
			call.FromIP = addr.IP.String()
			call.FromPort = addr.Port
		}
		if _, _, err := registrySvc.RecordCall(call, isTerminator); err != nil {
			slog.Warn("failed to record call", "frontend", frontend, "source", sourceKey, "error", err)
		}
	}
	motoSourceKey := func(source string, addr *net.UDPAddr) string {
		if idx := strings.LastIndex(source, ":"); idx >= 0 {
			if _, err := strconv.ParseUint(source[idx+1:], 10, 32); err == nil {
				return "moto:" + source[idx+1:]
			}
		}
		if addr != nil {
			return "moto:" + addr.IP.String()
		}
		return "moto:" + source
	}
	hyteraSourceKey := func(addr *net.UDPAddr) string {
		if addr == nil {
			return "hytera:unknown"
		}
		return "hytera:" + addr.IP.String()
	}
	mmdvmSourceKey := func(networkName string, pkt proto.Packet) string {
		if pkt.Repeater != 0 {
			key := fmt.Sprintf("mmdvm:%s:%d", networkName, pkt.Repeater)
			if _, ok := registrySvc.FindDevice(key); ok {
				return key
			}
		}
		return "mmdvm-upstream:" + networkName
	}
	sendToTarget := func(targetKey, sourceFrontend, sourceDeviceKey string, pkt proto.Packet) bool {
		if !forwardAllowed(targetKey) {
			slog.Debug("skipped forwarding to disabled device", "target", targetKey, "frontend", sourceFrontend)
			return false
		}
		switch {
		case strings.HasPrefix(targetKey, "moto:"):
			if ipscServer == nil {
				return false
			}
			peerID, err := strconv.ParseUint(strings.TrimPrefix(targetKey, "moto:"), 10, 32)
			if err != nil {
				return false
			}
			translator := getRouteMotoTranslator(sourceDeviceKey, targetKey)
			if translator == nil {
				return false
			}
			sent := false
			ingressGuard.MarkForwarded("moto", pkt)
			for _, ipscData := range translator.TranslateToIPSC(pkt) {
				if ipscServer.SendUserPacketToPeer(uint32(peerID), ipscData) {
					sent = true
				}
			}
			return sent
		case strings.HasPrefix(targetKey, "hytera:"):
			if hyteraServer == nil {
				return false
			}
			ingressGuard.MarkForwarded("hytera", pkt)
			if sourceFrontend == "moto" {
				return hyteraServer.SendPacketFromMotoTo(pkt, targetKey)
			}
			return hyteraServer.SendPacketTo(pkt, targetKey)
		case strings.HasPrefix(targetKey, "mmdvm-upstream:"):
			network := mmdvmUpstreams[targetKey]
			if network == nil {
				return false
			}
			return network.HandleTranslatedPacket(pkt, nil)
		case strings.HasPrefix(targetKey, "mmdvm:"):
			for _, server := range mmdvmServers {
				if server.SendPacketToPeer(pkt, targetKey) {
					return true
				}
			}
			return false
		default:
			return false
		}
	}
	routeGroupCall := func(sourceFrontend, sourceDeviceKey string, pkt proto.Packet) bool {
		if sourceDeviceKey == "" || pkt.Dst == 0 {
			slog.Debug("group call routing skipped: missing route key",
				"frontend", sourceFrontend,
				"sourceKey", sourceDeviceKey,
				"dst", pkt.Dst,
				"slot", pkt.Slot,
				"streamID", pkt.StreamID)
			return false
		}
		now := time.Now().UTC()
		slot := slotFromPacket(pkt)
		if shouldActivateDynamicGroup(pkt) {
			groupRouter.ActivateDynamic(sourceDeviceKey, slot, uint32(pkt.Dst), now)
		}
		targets := groupRouter.ResolveRoutes(sourceDeviceKey, uint32(pkt.Dst), now)
		if len(targets) == 0 {
			slog.Debug("group call route miss",
				"frontend", sourceFrontend,
				"sourceKey", sourceDeviceKey,
				"dst", pkt.Dst,
				"slot", slot,
				"streamID", pkt.StreamID,
				"frameType", pkt.FrameType,
				"dtypeOrVSeq", pkt.DTypeOrVSeq)
			return false
		}
		sortRouteTargets(sourceFrontend, targets)
		hyteraTargets := make([]routing.TargetRoute, 0, len(targets))
		sent := false
		for _, target := range targets {
			targetPkt, slotChanged := packetForTargetSlot(pkt, target.Slot)
			targetSent := false
			if strings.HasPrefix(target.DeviceKey, "hytera:") {
				hyteraTargets = append(hyteraTargets, target)
				continue
			}
			targetSent = sendToTarget(target.DeviceKey, sourceFrontend, sourceDeviceKey, targetPkt)
			slog.Debug("group call route target",
				"frontend", sourceFrontend,
				"sourceKey", sourceDeviceKey,
				"targetKey", target.DeviceKey,
				"targetSlot", target.Slot,
				"sourceSlot", slot,
				"slotChanged", slotChanged,
				"sent", targetSent,
				"dst", pkt.Dst,
				"streamID", pkt.StreamID,
				"frameType", pkt.FrameType,
				"dtypeOrVSeq", pkt.DTypeOrVSeq)
			if targetSent {
				sent = true
			}
		}
		if hyteraServer != nil && len(hyteraTargets) > 0 {
			for _, target := range hyteraTargets {
				targetPkt, slotChanged := packetForTargetSlot(pkt, target.Slot)
				targetSent := sendToTarget(target.DeviceKey, sourceFrontend, sourceDeviceKey, targetPkt)
				slog.Debug("group call route target",
					"frontend", sourceFrontend,
					"sourceKey", sourceDeviceKey,
					"targetKey", target.DeviceKey,
					"targetSlot", target.Slot,
					"sourceSlot", slot,
					"slotChanged", slotChanged,
					"sent", targetSent,
					"dst", pkt.Dst,
					"streamID", pkt.StreamID,
					"frameType", pkt.FrameType,
					"dtypeOrVSeq", pkt.DTypeOrVSeq)
				if targetSent {
					sent = true
				}
			}
		}
		return sent
	}
	legacyForward := func(frontend, sourceDeviceKey string, pkt proto.Packet, addr *net.UDPAddr) {
		if targetKey, ok := routePrivateCall(privateRouter, frontend, sourceDeviceKey, pkt, time.Now().UTC(), sendToTarget); ok {
			slog.Debug("private call routed via recent activity",
				"frontend", frontend,
				"sourceKey", sourceDeviceKey,
				"targetKey", targetKey,
				"dst", pkt.Dst,
				"src", pkt.Src,
				"slot", pkt.Slot,
				"streamID", pkt.StreamID)
			return
		}

		if frontend == "moto" {
			if hyteraServer != nil {
				ingressGuard.MarkForwarded("hytera", pkt)
				hyteraServer.SendPacketFromMoto(pkt)
			}
			_ = dispatchToMMDVM(pkt, addr)
			return
		}
		if frontend == "hytera" {
			if ipscServer != nil {
				ingressGuard.MarkForwarded("moto", pkt)
				translator := getRouteMotoTranslator("legacy:hytera", "legacy:moto")
				if translator == nil {
					return
				}
				for _, ipscData := range translator.TranslateToIPSC(pkt) {
					ipscServer.SendUserPacket(ipscData)
				}
			}
			_ = dispatchToMMDVM(pkt, addr)
			return
		}

		_ = dispatchToMMDVM(pkt, addr)
		if frontend != "moto" && ipscServer != nil {
			ingressGuard.MarkForwarded("moto", pkt)
			translator := getRouteMotoTranslator("legacy:"+frontend, "legacy:moto")
			if translator == nil {
				return
			}
			for _, ipscData := range translator.TranslateToIPSC(pkt) {
				ipscServer.SendUserPacket(ipscData)
			}
		}
		if frontend != "hytera" && hyteraServer != nil {
			ingressGuard.MarkForwarded("hytera", pkt)
			hyteraServer.SendPacket(pkt)
		}
	}
	handleIngress = func(frontend, source string, pkt proto.Packet, addr *net.UDPAddr) {
		recordSourceKey := source
		category := registry.CategoryMMDVM
		switch frontend {
		case "moto":
			recordSourceKey = motoSourceKey(source, addr)
			category = registry.CategoryMoto
		case "hytera":
			recordSourceKey = hyteraSourceKey(addr)
			category = registry.CategoryHytera
		}
		sourceKey := frontend + ":" + source
		if !ingressGuard.AllowIngress(sourceKey, pkt) {
			slog.Debug("dropped ingress packet by guard",
				"frontend", frontend, "source", source, "slot", pkt.Slot, "streamID", pkt.StreamID)
			return
		}
		dmrAudioPool.HandlePacket(frontend, recordSourceKey, pkt)
		if pkt.Src != 0 {
			privateRouter.Remember(uint32(pkt.Src), recordSourceKey, time.Now().UTC())
		}
		recordCall(frontend, recordSourceKey, category, pkt, addr)
		if pkt.GroupCall {
			_ = routeGroupCall(frontend, recordSourceKey, pkt)
			return
		}
		legacyForward(frontend, recordSourceKey, pkt, addr)
	}

	if cfg.Hytera.Enabled {
		hyteraServer = hytera.NewServer(cfg, m)
		hyteraServer.SetSendFilter(forwardAllowed)
		hyteraServer.SetNRLConfigResolver(func(sourceKey string) (hytera.NRLPeerConfig, bool) {
			dev, ok := registrySvc.FindDevice(sourceKey)
			if !ok {
				return hytera.NRLPeerConfig{}, false
			}
			serverAddr := strings.TrimSpace(dev.NRLServerAddr)
			if serverAddr == "" {
				return hytera.NRLPeerConfig{}, false
			}
			serverPort := dev.NRLServerPort
			if serverPort == 0 {
				serverPort = 60050
			}
			return hytera.NRLPeerConfig{
				ServerAddr:      serverAddr,
				ServerPort:      serverPort,
				SSID:            dev.NRLSSID,
				Callsign:        dev.Callsign,
				DMRID:           dev.DMRID,
				HyteraVoicePort: dev.Port,
			}, true
		})
		hyteraServer.SetNRLCallHandler(func(call hytera.NRLCallEvent) {
			record := registry.CallRecord{
				Frontend:       "nrl",
				SourceCategory: registry.CategoryHytera,
				SourceKey:      call.SourceKey,
				SourceName:     call.SourceName,
				SourceCallsign: call.SourceCallsign,
				SourceDMRID:    call.SourceDMRID,
				SrcID:          uint(call.SourceDMRID),
				DstID:          0,
				RepeaterID:     0,
				Slot:           1,
				CallType:       "analog",
				StreamID:       call.StreamID,
				FromIP:         call.FromIP,
				FromPort:       call.FromPort,
			}
			if dev, ok := registrySvc.FindDevice(call.HyteraSourceKey); ok {
				if cs := strings.TrimSpace(dev.Callsign); cs != "" {
					record.SourceName = cs
				} else if name := strings.TrimSpace(dev.Name); name != "" {
					record.SourceName = name
				}
			}
			if _, _, err := registrySvc.RecordCall(record, call.Ended); err != nil {
				slog.Warn("failed to record NRL call", "sourceKey", call.SourceKey, "ended", call.Ended, "error", err)
			}
		})
		hyteraServer.SetAnalogAudioHandler(func(event hytera.AnalogAudioEvent) {
			audioHub.Publish(audio.Chunk{
				StreamID:    event.StreamID,
				Frontend:    event.Frontend,
				SourceKey:   event.SourceKey,
				SourceDMRID: event.SourceDMRID,
				CallType:    "analog",
				SampleRate:  audio.SampleRate8000,
				Channels:    1,
				PCM:         audio.PCM16Bytes(event.PCM),
				Ended:       event.Ended,
			})
		})
		hyteraServer.SetPacketHandler(func(pkt proto.Packet, addr *net.UDPAddr) {
			handleIngress("hytera", addr.String(), pkt, addr)
		})
		hyteraServer.SetPeerUpdateHandler(func(addr *net.UDPAddr, p2pPort, dmrPort, rdacPort int, dmrid uint32) {
			if addr == nil {
				return
			}
			sourceKey := "hytera:" + addr.IP.String()
			if dmrid == 0 {
				if existing, ok := registrySvc.FindDevice(sourceKey); ok && existing.DMRID != 0 {
					dmrid = existing.DMRID
				}
			}
			extra := fmt.Sprintf(`{"p2pPort":%d,"dmrPort":%d`, p2pPort, dmrPort)
			if rdacPort > 0 {
				extra += fmt.Sprintf(`,"rdacPort":%d`, rdacPort)
			}
			extra += "}"
			_, err := registrySvc.UpsertDevice(registry.Device{
				Category:   registry.CategoryHytera,
				Protocol:   "hytera",
				SourceKey:  sourceKey,
				Name:       "Hytera Repeater",
				DMRID:      dmrid,
				Model:      "Hytera Repeater",
				IP:         addr.IP.String(),
				Port:       dmrPort,
				Status:     "online",
				Online:     true,
				LastSeenAt: time.Now().UTC(),
				ExtraJSON:  extra,
			})
			if err != nil {
				slog.Warn("failed to persist Hytera peer", "peer", addr.String(), "error", err)
			}
		})
		hyteraServer.SetPeerOfflineHandler(func(sourceKey string) {
			dev, ok := registrySvc.FindDevice(sourceKey)
			if !ok {
				return
			}
			dev.Online = false
			dev.Status = "offline"
			if _, err := registrySvc.UpsertDevice(dev); err != nil {
				slog.Warn("failed to set Hytera peer offline", "sourceKey", sourceKey, "error", err)
			}
		})
		err = hyteraServer.Start()
		if err != nil {
			return fmt.Errorf("failed to start Hytera server: %w", err)
		}
	}

	if cfg.IPSC.Enabled {
		ipscServer = ipsc.NewIPSCServer(cfg, m)
		ipscServer.SetSendFilter(forwardAllowed)
		ipscServer.SetPeerUpdateHandler(func(peer ipsc.Peer) {
			ip := ""
			port := 0
			if peer.Addr != nil {
				ip = peer.Addr.IP.String()
				port = peer.Addr.Port
			}
			_, err := registrySvc.UpsertDevice(registry.Device{
				Category:   registry.CategoryMoto,
				Protocol:   "ipsc",
				SourceKey:  fmt.Sprintf("moto:%d", peer.ID),
				Name:       fmt.Sprintf("Moto %d", peer.ID),
				DMRID:      peer.ID,
				Model:      "Motorola IPSC Repeater",
				IP:         ip,
				Port:       port,
				Status:     "online",
				Online:     true,
				LastSeenAt: peer.LastSeen.UTC(),
				ExtraJSON:  fmt.Sprintf(`{"mode":%d,"flags":"% X","keepAlive":%d}`, peer.Mode, peer.Flags, peer.KeepAliveReceived),
			})
			if err != nil {
				slog.Warn("failed to persist moto peer", "peer", peer.ID, "error", err)
			}
		})
		ipscServer.SetPeerOfflineHandler(func(sourceKey string) {
			dev, ok := registrySvc.FindDevice(sourceKey)
			if !ok {
				return
			}
			dev.Online = false
			dev.Status = "offline"
			if _, err := registrySvc.UpsertDevice(dev); err != nil {
				slog.Warn("failed to set IPSC peer offline", "sourceKey", sourceKey, "error", err)
			}
		})
		ipscServer.SetBurstHandler(func(packetType byte, data []byte, addr *net.UDPAddr) {
			source := addr.String()
			if len(data) >= 5 {
				peerID := uint(data[1])<<24 | uint(data[2])<<16 | uint(data[3])<<8 | uint(data[4])
				source = fmt.Sprintf("%s:%d", addr.String(), peerID)
			}
			tr := getMotoTranslator(source)
			if tr == nil {
				return
			}
			packets := tr.TranslateToMMDVM(packetType, data)
			for _, pkt := range packets {
				handleIngress("moto", source, pkt, addr)
			}
		})
		err = ipscServer.Start()
		if err != nil {
			return fmt.Errorf("failed to start IPSC server: %w", err)
		}
	}

	for _, network := range mmdvmNetworks {
		if ipscServer != nil {
			network.SetIPSCHandler(ipscServer.SendUserPacket)
		}
		networkName := network.Name()
		network.SetPacketHandler(func(packet proto.Packet) {
			handleIngress("mmdvm", mmdvmSourceKey(networkName, packet), packet, nil)
		})
	}

	stop := func(sig os.Signal) {
		slog.Info("received signal, shutting down...", "signal", sig.String())

		if metricsSrv != nil {
			if err := metricsSrv.Shutdown(context.Background()); err != nil {
				slog.Error("Error shutting down metrics server", "error", err)
			}
		}
		if webSrv != nil {
			if err := webSrv.Shutdown(context.Background()); err != nil {
				slog.Error("Error shutting down web server", "error", err)
			}
		}

		if ipscServer != nil {
			ipscServer.Stop()
		}
		if hyteraServer != nil {
			hyteraServer.Stop()
		}
		for _, network := range mmdvmNetworks {
			network.Stop()
		}
		if err := registrySvc.Close(); err != nil {
			slog.Error("Error closing registry service", "error", err)
		}
	}

	shutdown.AddWithParam(stop)
	shutdown.Listen(syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)

	return nil
}

func sortRouteTargets(sourceFrontend string, targets []routing.TargetRoute) {
	sort.SliceStable(targets, func(i, j int) bool {
		pi := routeTargetPriority(sourceFrontend, targets[i].DeviceKey)
		pj := routeTargetPriority(sourceFrontend, targets[j].DeviceKey)
		if pi != pj {
			return pi < pj
		}
		if targets[i].DeviceKey != targets[j].DeviceKey {
			return targets[i].DeviceKey < targets[j].DeviceKey
		}
		return targets[i].Slot < targets[j].Slot
	})
}

func routeTargetPriority(sourceFrontend, deviceKey string) int {
	switch sourceFrontend {
	case "moto":
		switch {
		case strings.HasPrefix(deviceKey, "hytera:"):
			return 0
		case strings.HasPrefix(deviceKey, "mmdvm:"):
			return 1
		case strings.HasPrefix(deviceKey, "mmdvm-upstream:"):
			return 2
		case strings.HasPrefix(deviceKey, "moto:"):
			return 3
		}
	case "hytera":
		switch {
		case strings.HasPrefix(deviceKey, "moto:"):
			return 0
		case strings.HasPrefix(deviceKey, "mmdvm:"):
			return 1
		case strings.HasPrefix(deviceKey, "mmdvm-upstream:"):
			return 2
		case strings.HasPrefix(deviceKey, "hytera:"):
			return 3
		}
	case "mmdvm":
		switch {
		case strings.HasPrefix(deviceKey, "moto:"):
			return 0
		case strings.HasPrefix(deviceKey, "hytera:"):
			return 1
		case strings.HasPrefix(deviceKey, "mmdvm:"):
			return 2
		case strings.HasPrefix(deviceKey, "mmdvm-upstream:"):
			return 3
		}
	}
	return 10
}

func packetForTargetSlot(pkt proto.Packet, targetSlot routing.Slot) (proto.Packet, bool) {
	targetPkt := pkt
	wantSlot2 := targetSlot == routing.Slot2
	changed := targetPkt.Slot != wantSlot2
	targetPkt.Slot = wantSlot2
	return targetPkt, changed
}

func shouldActivateDynamicGroup(pkt proto.Packet) bool {
	if !pkt.GroupCall || pkt.Dst == 0 {
		return false
	}
	return pkt.FrameType == 2 && pkt.DTypeOrVSeq == 1
}

func loadDMRIDResolver(cfg *config.Config) (*dmrid.Resolver, error) {
	candidates := make([]string, 0, 4)
	if cfg.DMRIDDB.Path != "" {
		candidates = append(candidates, cfg.DMRIDDB.Path)
	} else {
		candidates = append(candidates, "dmrid.csv", "dmrid.txt", "radioid.csv", "radioid.txt")
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			resolver, err := dmrid.Load(candidate)
			if err != nil {
				return nil, err
			}
			slog.Info("Loaded DMR ID database", "path", candidate)
			return resolver, nil
		} else if cfg.DMRIDDB.Path != "" {
			return nil, err
		}
	}
	return nil, nil
}
