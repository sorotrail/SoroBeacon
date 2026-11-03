import {
  SoroBeaconClient,
  SoroBeaconAPIError,
  toBigInt,
  type Channel,
  type ChannelInput,
  type Monitor,
  type Alert,
} from "../src/index.ts";

const CONTRACT_USDC =
  "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC";
const CONTRACT_DEX =
  "CA3D5KRYM6CB7OWQ6TWYRR3Z4T7GNZLKERYNZGGA5SOAOPIFY6YQGAXE";

export async function runUsageExample(
  baseUrl: string,
  token = "dev-token",
): Promise<void> {
  const client = new SoroBeaconClient({ baseUrl, token });

  // 1. Create a notification channel. ChannelInput requires `config` on write,
  //    while the returned Channel type omits `config` (config?: never) because
  //    the server redacts secrets on read.
  const channelInput: ChannelInput = {
    name: "ops-webhook",
    type: "webhook",
    config: {
      url: "https://hooks.example.com/sorobeacon",
    },
    enabled: true,
  };
  const channel: Channel = await client.createChannel(channelInput);
  if ("config" in channel && channel.config !== undefined) {
    throw new Error("expected channel.config to be redacted on read");
  }

  // 2. Create monitors and attach an evaluation rule.
  const m1: Monitor = await client.createMonitor({
    name: "usdc-transfer-watch",
    contract_ids: [CONTRACT_USDC],
    enabled: true,
    channel_ids: [channel.id],
    tags: ["treasury", "usdc"],
  });
  const m2: Monitor = await client.createMonitor({
    name: "dex-swap-watch",
    contract_ids: [CONTRACT_DEX],
    enabled: true,
  });

  const rule = await client.createRule(m1.id, {
    type: "event_emitted",
    params: { event_name: "transfer" },
    enabled: true,
  });

  // 3. Iterate across pages transparently using the async pagination helper.
  const seenMonitors: Monitor[] = [];
  for await (const mon of client.paginateMonitors({ limit: 1 })) {
    seenMonitors.push(mon);
  }
  if (seenMonitors.length < 2) {
    throw new Error(
      `expected paginateMonitors to walk at least 2 monitors, got ${seenMonitors.length}`,
    );
  }

  // 4. Walk alerts and inspect structured JSON payloads and wide 64-bit/128-bit
  //    integers without IEEE-754 precision loss.
  const seenAlerts: Alert[] = [];
  for await (const alert of client.paginateAlerts({
    monitor_id: m1.id,
    limit: 1,
  })) {
    seenAlerts.push(alert);
    const alertIdBig = toBigInt(alert.id);
    if (alertIdBig <= 0n) {
      throw new Error(`expected positive alert id, got ${alert.id}`);
    }
    if (typeof alert.payload !== "object" || alert.payload === null) {
      throw new Error("expected alert.payload to be a structured JSON object");
    }
  }

  // 5. Exercise typed non-2xx error handling and taxonomy helpers.
  let caughtNotFound = false;
  try {
    await client.getMonitor(999999);
  } catch (err) {
    if (SoroBeaconAPIError.is(err) && err.isNotFound()) {
      caughtNotFound = true;
    } else {
      throw err;
    }
  }
  if (!caughtNotFound) {
    throw new Error("expected getMonitor(999999) to throw SoroBeaconAPIError (NOT_FOUND)");
  }

  // 6. Query version, stats, and clean up created resources.
  const version = await client.getVersion();
  if (typeof version.version !== "string") {
    throw new Error("expected version.version string");
  }

  await client.deleteRule(m1.id, rule.id);
  await client.deleteMonitor(m2.id);
  await client.deleteMonitor(m1.id);
  await client.deleteChannel(channel.id);
}

// Execute when invoked directly (e.g. `SOROBEACON_URL=http://... node examples/usage.ts`).
const nodeProcess = (
  globalThis as {
    process?: {
      env?: Record<string, string | undefined>;
      exit?: (code: number) => void;
    };
  }
).process;
const envUrl = nodeProcess?.env?.SOROBEACON_URL;
if (envUrl) {
  runUsageExample(envUrl, nodeProcess?.env?.SOROBEACON_TOKEN ?? "dev-token").catch(
    (err) => {
      console.error(err);
      nodeProcess?.exit?.(1);
    },
  );
}
