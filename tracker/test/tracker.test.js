import assert from "node:assert/strict";
import { Buffer } from "node:buffer";
import test from "node:test";

import worker, { buildSuccessBody, MAX_SWARM_PEERS, Swarm } from "../src/index.js";

test("announces require a Bork user agent", async () => {
  const response = await worker.fetch(new Request("https://tracker.test/announce"), {});

  assert.equal(response.status, 403);
  assert.match(await response.text(), /user agent must start with Bork/);
});

test("announce query processing has a fixed limit", async () => {
  const request = new Request(`https://tracker.test/announce?${"x".repeat(4097)}`, {
    headers: { "User-Agent": "Bork-test" },
  });

  const response = await worker.fetch(request, {});

  assert.match(await response.text(), /announce query is too long/);
});

test("the declared ip overrides the request source address", async () => {
  let forwarded;
  const env = {
    SWARMS: {
      idFromName: (name) => name,
      get: () => ({
        async fetch(request) {
          forwarded = await request.json();
          return new Response("ok");
        },
      }),
    },
  };
  const infoHash = Uint8Array.from({ length: 20 }, (_, index) => index);
  const peerID = Uint8Array.from({ length: 20 }, (_, index) => (240 + index) & 0xff);
  const query = new URLSearchParams({
    port: "6881",
    numwant: "32",
    uploaded: "0",
    downloaded: "0",
    left: "0",
    ip: "10.20.30.40",
  });
  const request = new Request(
    `https://tracker.test/announce?info_hash=${percentEncode(infoHash)}&peer_id=${percentEncode(peerID)}&${query}`,
    {
      headers: { "CF-Connecting-IP": "198.51.100.7", "User-Agent": "Bork-test" },
    },
  );

  const response = await worker.fetch(request, env);

  assert.equal(response.status, 200);
  assert.equal(forwarded.infoHash, "000102030405060708090a0b0c0d0e0f10111213");
  assert.equal(forwarded.peerID, "f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff00010203");
  assert.equal(forwarded.address, "0a141e28");
  assert.equal(forwarded.externalIP, "c6336407");
});

test("the compact response includes declared IPv4 and IPv6 peers", async () => {
  const storage = new MemoryStorage();
  const swarm = new Swarm({ storage });
  await announce(swarm, "01".repeat(20), "0a010203", 6881);
  await announce(swarm, "02".repeat(20), "20010db8000000000000000000000001", 7000);

  const response = await announce(swarm, "03".repeat(20), "c0000201", 7001);
  const body = Buffer.from(await response.arrayBuffer());

  assert.equal(body.includes(Uint8Array.of(10, 1, 2, 3, 0x1a, 0xe1)), true);
  assert.equal(
    body.includes(Uint8Array.of(0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0x1b, 0x58)),
    true,
  );
});

test("success dictionaries use canonical bencode key order", () => {
  const body = buildSuccessBody(Uint8Array.of(192, 0, 2, 1), []);
  const text = new TextDecoder().decode(body);

  assert.equal(text, "d11:external ip4:\uFFFD\u0000\u0002\u00018:intervali300e5:peers0:6:peers60:e");
});

test("stopped and expired peers release all swarm storage", async () => {
  const storage = new MemoryStorage();
  const swarm = new Swarm({ storage });
  await announce(swarm, "01".repeat(20), "0a010203", 6881);
  await announce(swarm, "02".repeat(20), "0a010204", 6882);

  await announce(swarm, "01".repeat(20), "0a010203", 6881, true);
  assert.equal(storage.peers.length, 1);

  await announce(swarm, "02".repeat(20), "0a010204", 6882, true);
  assert.equal(storage.peers, undefined);
  assert.equal(storage.deleteAllCalls, 1);

  await announce(swarm, "02".repeat(20), "0a010204", 6882);
  storage.peers[0].seenAt = Date.now() - 900_000;
  await swarm.alarm();

  assert.equal(storage.peers, undefined);
  assert.equal(storage.alarmAt, null);
  assert.ok(storage.deleteAllCalls >= 2);
});

test("a swarm has a fixed registration and read limit", async () => {
  const storage = new MemoryStorage();
  const now = Date.now();
  storage.peers = [];
  for (let index = 0; index < MAX_SWARM_PEERS; index += 1) {
    storage.peers.push({
      peerID: index.toString(16).padStart(40, "0"),
      address: "0a010203",
      port: 6881,
      seenAt: now,
    });
  }

  await announce(new Swarm({ storage }), "ff".repeat(20), "0a010204", 6882);

  assert.equal(storage.peers.length, MAX_SWARM_PEERS);
  assert.equal(storage.peers.some((peer) => peer.peerID === "0".repeat(40)), false);
  assert.equal(storage.peers.some((peer) => peer.peerID === "ff".repeat(20) && peer.address === "0a010204"), true);
  assert.equal(storage.getCalls, 1);
});

async function announce(swarm, peerID, address, port, stopped = false) {
  return swarm.fetch(
    new Request("https://swarm/announce", {
      method: "POST",
      body: JSON.stringify({
        peerID,
        address,
        externalIP: address,
        port,
        numWant: 32,
        stopped,
      }),
    }),
  );
}

function percentEncode(value) {
  return Array.from(value, (byte) => `%${byte.toString(16).padStart(2, "0")}`).join("");
}

class MemoryStorage {
  constructor() {
    this.peers = undefined;
    this.alarmAt = null;
    this.deleteAllCalls = 0;
    this.getCalls = 0;
  }

  async get() {
    this.getCalls += 1;
    return this.peers;
  }

  async put(key, value) {
    assert.equal(key, "peers");
    this.peers = value;
  }

  async deleteAll() {
    this.deleteAllCalls += 1;
    this.peers = undefined;
    this.alarmAt = null;
  }

  async setAlarm(time) {
    this.alarmAt = time;
  }
}
