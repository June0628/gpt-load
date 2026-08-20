package handler

import (
	"net"
	"time"

	"gpt-load/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// NetworkInfoResponse 网络信息响应
type NetworkInfoResponse struct {
	LocalIPs    []string `json:"local_ips"`    // 内网 IP 列表
	OutboundIP string   `json:"outbound_ip"`  // 外网 IP
	ServerPort  int      `json:"server_port"`  // 服务端口
}

// GetNetworkInfo 返回服务器的网络信息（内网 IP、外网 IP、端口）
func (s *Server) GetNetworkInfo(c *gin.Context) {
	localIPs := getLocalIPs()
	outboundIP := getOutboundIP()

	serverConfig := s.config.GetEffectiveServerConfig()

	response.Success(c, NetworkInfoResponse{
		LocalIPs:    localIPs,
		OutboundIP:  outboundIP,
		ServerPort:  serverConfig.Port,
	})
}

// getLocalIPs 获取所有非回环的内网 IPv4 地址
func getLocalIPs() []string {
	var ips []string
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		logrus.WithError(err).Warn("Failed to get network interface addresses")
		return ips
	}
	for _, addr := range interfaces {
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				ips = append(ips, ipNet.IP.String())
			}
		}
	}
	return ips
}

// outboundProbeTargets 用于探测出口 IP 的目标地址列表
// 使用 UDP 模拟外网访问（不实际发送数据包），只要能路由到即可获取本机出口 IP
var outboundProbeTargets = []string{
	"8.8.8.8:80",
	"1.1.1.1:80",
	"114.114.114.114:80",
}

// getOutboundIP 获取访问外网时使用的出口 IP
func getOutboundIP() string {
	for _, target := range outboundProbeTargets {
		conn, err := net.DialTimeout("udp", target, 3*time.Second)
		if err != nil {
			continue
		}
		localAddr := conn.LocalAddr().(*net.UDPAddr)
		conn.Close()
		return localAddr.IP.String()
	}
	logrus.Warn("Failed to determine outbound IP: all probe targets unreachable")
	return ""
}
