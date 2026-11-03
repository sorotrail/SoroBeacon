package notify

// ReceiptCapability classifies the delivery guarantee and acknowledgement
// feedback offered by a notification channel type.
type ReceiptCapability string

const (
	// CapabilityPagingAck: The provider natively tracks human incident acknowledgement (e.g. PagerDuty, Opsgenie).
	CapabilityPagingAck ReceiptCapability = "paging_ack"
	// CapabilityDeliveryReceipt: The provider tracks device or carrier delivery status receipts (e.g. Twilio SMS, Pushover).
	CapabilityDeliveryReceipt ReceiptCapability = "delivery_receipt"
	// CapabilityHTTPAcceptance: The endpoint returns HTTP 2xx acceptance, which confirms the relay or server
	// received the payload, but provides no downstream arrival or viewing guarantee (e.g. Webhook, Slack, Discord, Email).
	CapabilityHTTPAcceptance ReceiptCapability = "http_acceptance"
)

// ChannelReceiptCapability returns the receipt capability for a given channel type.
func ChannelReceiptCapability(channelType string) ReceiptCapability {
	switch channelType {
	case TypePagerDuty, TypeOpsgenie:
		return CapabilityPagingAck
	case TypeTwilio, TypePushover:
		return CapabilityDeliveryReceipt
	default:
		return CapabilityHTTPAcceptance
	}
}

// ReceiptCapabilityLabel returns a concise human-readable description for dashboard display.
func ReceiptCapabilityLabel(channelType string) string {
	switch ChannelReceiptCapability(channelType) {
	case CapabilityPagingAck:
		return "native paging acknowledgement"
	case CapabilityDeliveryReceipt:
		return "provider delivery receipt"
	default:
		return "HTTP acceptance only"
	}
}
