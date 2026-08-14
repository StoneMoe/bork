const ANNOUNCE_INTERVAL_SECONDS = 300;
const PEER_LIFETIME_MILLISECONDS = 900_000;
const MAX_RETURNED_PEERS = 50;
export const MAX_SWARM_PEERS = 500;
const MAX_QUERY_LENGTH = 4096;
const PEERS_STORAGE_KEY = "peers";
const textEncoder = new TextEncoder();

const trackerHeaders = {
  "cache-control": "no-store",
  "content-type": "text/plain",
};

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (request.method === "GET" && url.pathname === "/") {
      return new Response("http bittorrent tracker\n", { headers: trackerHeaders });
    }
    if (request.method !== "GET" || url.pathname !== "/announce") {
      return new Response("not found\n", { status: 404 });
    }
    if (!request.headers.get("user-agent")?.startsWith("Bork")) {
      return failureResponse("user agent must start with Bork", 403);
    }

    let announce;
    try {
      announce = parseAnnounceRequest(request);
    } catch (error) {
      return failureResponse(error.message);
    }

    const swarm = env.SWARMS.get(env.SWARMS.idFromName(announce.infoHash));
    return swarm.fetch(
      new Request("https://swarm/announce", {
        method: "POST",
        body: JSON.stringify(announce),
      }),
    );
  },
};

export class Swarm {
  constructor(state) {
    this.storage = state.storage;
  }

  async fetch(request) {
    const announce = await request.json();
    const now = Date.now();
    let peers = await this.activePeers(now);

    peers = peers.filter((peer) => peer.peerID !== announce.peerID || peer.address !== announce.address);
    if (!announce.stopped) {
      if (peers.length >= MAX_SWARM_PEERS) {
        // Announcements move peers to the end, so the first peer is least recently seen.
        peers.shift();
      }

      peers.push({
        peerID: announce.peerID,
        address: announce.address,
        port: announce.port,
        seenAt: now,
      });
    }
    await this.savePeers(peers);

    // Rotate the stable storage order so large swarms do not always return the
    // same first page of peers.
    const selected = selectPeers(peers, announce.peerID, announce.address, announce.numWant);
    return new Response(buildSuccessBody(bytesFromHex(announce.externalIP), selected), { headers: trackerHeaders });
  }

  async alarm() {
    await this.savePeers(await this.activePeers(Date.now()));
  }

  async activePeers(now) {
    const peers = (await this.storage.get(PEERS_STORAGE_KEY)) ?? [];
    return peers.filter((peer) => now - peer.seenAt < PEER_LIFETIME_MILLISECONDS);
  }

  async savePeers(peers) {
    if (peers.length === 0) {
      // Removing all storage closes the lifecycle for an empty swarm instead
      // of leaving Durable Object metadata behind.
      await this.storage.deleteAll();
      return;
    }
    await this.storage.put(PEERS_STORAGE_KEY, peers);
    const expiresAt = Math.min(...peers.map((peer) => peer.seenAt + PEER_LIFETIME_MILLISECONDS));
    await this.storage.setAlarm(expiresAt);
  }
}

function parseAnnounceRequest(request) {
  const url = new URL(request.url);
  if (url.search.length > MAX_QUERY_LENGTH) {
    throw new Error("announce query is too long");
  }
  const params = url.searchParams;
  const infoHash = requiredBinaryParameter(url.search, "info_hash", 20);
  const peerID = requiredBinaryParameter(url.search, "peer_id", 20);
  const port = integerParameter(params, "port", 1, 65_535);
  const numWant =
    params.get("numwant") === "-1"
      ? MAX_RETURNED_PEERS
      : Math.min(integerParameter(params, "numwant", 0, 2_147_483_647, MAX_RETURNED_PEERS), MAX_RETURNED_PEERS);
  requiredByteCount(params, "uploaded");
  requiredByteCount(params, "downloaded");
  requiredByteCount(params, "left");
  const event = params.get("event") ?? "";
  if (!["", "started", "completed", "stopped"].includes(event)) {
    throw new Error("event must be started, completed, or stopped");
  }
  const declaredAddress = optionalAddress(params.get("ip"), "ip");
  const sourceAddress = optionalAddress(request.headers.get("CF-Connecting-IP"), "source ip");
  const registeredAddress = declaredAddress ?? sourceAddress;
  if (registeredAddress === null) {
    throw new Error("ip is required when the source address is unavailable");
  }

  return {
    infoHash: bytesToHex(infoHash),
    peerID: bytesToHex(peerID),
    port,
    numWant,
    stopped: event === "stopped",
    address: registeredAddress,
    externalIP: sourceAddress ?? registeredAddress,
  };
}

function requiredBinaryParameter(search, name, expectedLength) {
  const encoded = rawQueryParameter(search, name);
  if (encoded === null) {
    throw new Error(`${name} is required`);
  }
  const value = decodeQueryBytes(encoded);
  if (value.length !== expectedLength) {
    throw new Error(`${name} must be ${expectedLength} bytes`);
  }
  return value;
}

function rawQueryParameter(search, name) {
  for (const field of search.slice(1).split("&")) {
    const separator = field.indexOf("=");
    if (separator >= 0 && field.slice(0, separator) === name) {
      return field.slice(separator + 1);
    }
  }
  return null;
}

// URLSearchParams turns arbitrary tracker bytes into UTF-8 text. Decode these
// two binary fields directly so every possible info hash and peer ID survives.
function decodeQueryBytes(encoded) {
  const output = new Uint8Array(encoded.length);
  let length = 0;
  for (let index = 0; index < encoded.length; index += 1) {
    const character = encoded[index];
    if (character === "+") {
      output[length++] = 0x20;
      continue;
    }
    if (character === "%") {
      const pair = encoded.slice(index + 1, index + 3);
      if (!/^[0-9a-f]{2}$/i.test(pair)) {
        throw new Error("invalid percent encoding");
      }
      output[length++] = Number.parseInt(pair, 16);
      index += 2;
      continue;
    }
    if (encoded.charCodeAt(index) > 0x7f) {
      throw new Error("binary query values must be percent encoded");
    }
    output[length++] = encoded.charCodeAt(index);
  }
  return output.slice(0, length);
}

function integerParameter(params, name, minimum, maximum, fallback) {
  const encoded = params.get(name);
  if (encoded === null && fallback !== undefined) {
    return fallback;
  }
  if (encoded === null || !/^\d+$/.test(encoded)) {
    throw new Error(`${name} must be an integer`);
  }
  const value = Number(encoded);
  if (value < minimum || value > maximum) {
    throw new Error(`${name} is out of range`);
  }
  return value;
}

function requiredByteCount(params, name) {
  const value = params.get(name);
  if (value === null || !/^\d+$/.test(value)) {
    throw new Error(`${name} must be a non-negative integer`);
  }
}

function optionalAddress(value, name) {
  if (value === null || value === "") {
    return null;
  }
  const bytes = parseIPv4(value) ?? parseIPv6(value);
  if (bytes === null) {
    throw new Error(`${name} must be an IPv4 or IPv6 address`);
  }
  return bytesToHex(bytes);
}

function parseIPv4(value) {
  const parts = value.split(".");
  if (parts.length !== 4) {
    return null;
  }
  const bytes = new Uint8Array(4);
  for (let index = 0; index < parts.length; index += 1) {
    if (!/^\d{1,3}$/.test(parts[index])) {
      return null;
    }
    const part = Number(parts[index]);
    if (part > 255) {
      return null;
    }
    bytes[index] = part;
  }
  return bytes;
}

function parseIPv6(value) {
  if (!value.includes(":")) {
    return null;
  }
  const halves = value.split("::");
  if (halves.length > 2) {
    return null;
  }
  const left = parseIPv6Half(halves[0], halves.length === 1);
  const right = parseIPv6Half(halves[1] ?? "", true);
  if (left === null || right === null) {
    return null;
  }
  const gap = 8 - left.length - right.length;
  if (halves.length === 1 ? gap !== 0 : gap <= 0) {
    return null;
  }
  return ipv6WordsToBytes([...left, ...new Array(gap).fill(0), ...right]);
}

function parseIPv6Half(value, allowIPv4Tail) {
  if (value === "") {
    return [];
  }
  const parts = value.split(":");
  const words = [];
  for (let index = 0; index < parts.length; index += 1) {
    const ipv4 = parseIPv4Tail(parts, index, allowIPv4Tail);
    if (ipv4 !== null) {
      words.push((ipv4[0] << 8) | ipv4[1], (ipv4[2] << 8) | ipv4[3]);
      continue;
    }
    if (!/^[0-9a-f]{1,4}$/i.test(parts[index])) {
      return null;
    }
    words.push(Number.parseInt(parts[index], 16));
  }
  return words;
}

function parseIPv4Tail(parts, index, allowed) {
  if (!parts[index].includes(".")) {
    return null;
  }
  if (!allowed || index !== parts.length - 1) {
    return null;
  }
  return parseIPv4(parts[index]);
}

function ipv6WordsToBytes(words) {
  const bytes = new Uint8Array(16);
  for (let index = 0; index < words.length; index += 1) {
    bytes[index * 2] = words[index] >> 8;
    bytes[index * 2 + 1] = words[index] & 0xff;
  }
  return bytes;
}

function selectPeers(active, currentPeerID, currentAddress, limit) {
  const available = active.filter((peer) => peer.peerID !== currentPeerID);
  if (available.length <= limit) {
    return available;
  }
  const offset = Number.parseInt(currentAddress.slice(-8), 16) % available.length;
  return Array.from({ length: limit }, (_, index) => available[(offset + index) % available.length]);
}

export function buildSuccessBody(externalIP, peers) {
  const compact4 = compactPeers(peers, 4);
  const compact6 = compactPeers(peers, 16);
  return joinBytes([
    textEncoder.encode("d11:external ip"),
    bencodedBytes(externalIP),
    textEncoder.encode(`8:intervali${ANNOUNCE_INTERVAL_SECONDS}e5:peers`),
    bencodedBytes(compact4),
    textEncoder.encode("6:peers6"),
    bencodedBytes(compact6),
    textEncoder.encode("e"),
  ]);
}

function compactPeers(peers, addressLength) {
  const chunks = [];
  for (const peer of peers) {
    const address = bytesFromHex(peer.address);
    if (address.length === addressLength) {
      chunks.push(address, Uint8Array.of(peer.port >> 8, peer.port & 0xff));
    }
  }
  return joinBytes(chunks);
}

function bencodedBytes(value) {
  return joinBytes([textEncoder.encode(`${value.length}:`), value]);
}

function failureResponse(message, status = 200) {
  const reason = textEncoder.encode(message);
  const body = joinBytes([
    textEncoder.encode("d14:failure reason"),
    bencodedBytes(reason),
    textEncoder.encode("e"),
  ]);
  return new Response(body, { status, headers: trackerHeaders });
}

function joinBytes(chunks) {
  const length = chunks.reduce((total, chunk) => total + chunk.length, 0);
  const joined = new Uint8Array(length);
  let offset = 0;
  for (const chunk of chunks) {
    joined.set(chunk, offset);
    offset += chunk.length;
  }
  return joined;
}

function bytesToHex(bytes) {
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("");
}

function bytesFromHex(hex) {
  const bytes = new Uint8Array(hex.length / 2);
  for (let index = 0; index < bytes.length; index += 1) {
    bytes[index] = Number.parseInt(hex.slice(index * 2, index * 2 + 2), 16);
  }
  return bytes;
}
