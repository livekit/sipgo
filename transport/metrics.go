package transport

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	bytesPacketSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace:   "sipgo",
		Subsystem:   "transport",
		Name:        "packet_size_bytes",
		Help:        "Size of sent and received SIP packets",
		ConstLabels: nil,
		Buckets: []float64{
			250, 500, 1000,
			1100, 1200, 1300,
			1400, 1450,
			1500, // typical MTU
			1550, 1600,
			1700, 1800,
			1900, 2000,
			3000, 4000,
		},
	}, []string{"transport", "type"})

	parserReset = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgo",
		Subsystem: "transport",
		Name:      "parser_reset_total",
		Help:      "Stream parsers reset without closing the connection, by reason",
	}, []string{"transport", "reason"})

	connectionOpened = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgo",
		Subsystem: "transport",
		Name:      "connection_opened_total",
		Help:      "Connections opened by the transport, dialed and accepted",
	}, []string{"transport"})

	connectionClosed = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "sipgo",
		Subsystem: "transport",
		Name:      "connection_closed_total",
		Help:      "Connections closed by the transport, by reason",
	}, []string{"transport", "reason"})

	connectionsOpen = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sipgo",
		Subsystem: "transport",
		Name:      "connections_open",
		Help:      "Connections currently open, by transport",
	}, []string{"transport"})
)
