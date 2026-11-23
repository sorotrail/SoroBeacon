package notify

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChannelReceiptCapability(t *testing.T) {
	assert.Equal(t, CapabilityPagingAck, ChannelReceiptCapability(TypePagerDuty))
	assert.Equal(t, CapabilityPagingAck, ChannelReceiptCapability(TypeOpsgenie))

	assert.Equal(t, CapabilityDeliveryReceipt, ChannelReceiptCapability(TypeTwilio))
	assert.Equal(t, CapabilityDeliveryReceipt, ChannelReceiptCapability(TypePushover))

	assert.Equal(t, CapabilityHTTPAcceptance, ChannelReceiptCapability(TypeWebhook))
	assert.Equal(t, CapabilityHTTPAcceptance, ChannelReceiptCapability(TypeSlack))
	assert.Equal(t, CapabilityHTTPAcceptance, ChannelReceiptCapability(TypeDiscord))
	assert.Equal(t, CapabilityHTTPAcceptance, ChannelReceiptCapability(TypeTelegram))
	assert.Equal(t, CapabilityHTTPAcceptance, ChannelReceiptCapability(TypeEmail))
	assert.Equal(t, CapabilityHTTPAcceptance, ChannelReceiptCapability(TypeMatrix))

	assert.Equal(t, "native paging acknowledgement", ReceiptCapabilityLabel(TypePagerDuty))
	assert.Equal(t, "provider delivery receipt", ReceiptCapabilityLabel(TypeTwilio))
	assert.Equal(t, "HTTP acceptance only", ReceiptCapabilityLabel(TypeWebhook))
}
