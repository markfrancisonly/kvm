import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { rolldown } from "rolldown";

// The browser-side frame codec must agree with internal/hostagent/frame.go.
const entry = fileURLToPath(new URL("../src/hostagent/frame.ts", import.meta.url));
const bundle = await rolldown({ input: entry });
const { output } = await bundle.generate({ format: "es" });
await bundle.close();
const { encodeFrame, decodeFrame, parseAck, blobDataPayload, crc32, FrameType, MAX_PAYLOAD } =
  await import(`data:text/javascript;base64,${Buffer.from(output[0].code).toString("base64")}`);

// CRC-32 check value.
assert.equal(crc32(new TextEncoder().encode("123456789")), 0xcbf43926);

// Byte-exact against the Go encoder: a TEXT frame, clipboard flag, device
// sequence number 7, payload "hi".
const text = encodeFrame({
  type: FrameType.Text,
  flags: 0x01,
  seq: 0x80000007,
  payload: new TextEncoder().encode("hi"),
});
assert.deepEqual(
  [...text.subarray(0, 16)],
  [0x4a, 0x4b, 0x56, 0x4d, 1, 0x10, 0x01, 0, 0x07, 0, 0, 0x80, 2, 0, 0, 0],
);
assert.deepEqual([...text.subarray(16, 18)], [0x68, 0x69]);
assert.equal(text.length, 16 + 2 + 4);

// Round trip.
const payload = blobDataPayload(3, 16384, new Uint8Array([9, 8, 7]));
const frame = decodeFrame(encodeFrame({ type: FrameType.BlobData, seq: 42, payload }));
assert.ok(frame);
assert.equal(frame.type, FrameType.BlobData);
assert.equal(frame.seq, 42);
assert.deepEqual([...frame.payload], [3, 0, 0, 0, 0, 0x40, 0, 0, 9, 8, 7]);

// Corruption and trailing bytes are rejected.
const good = encodeFrame({ type: FrameType.Ping, seq: 1, payload: new Uint8Array() });
const bad = Uint8Array.from(good);
bad[bad.length - 1] ^= 0xff;
assert.equal(decodeFrame(bad), null);
assert.equal(decodeFrame(new Uint8Array([...good, 0])), null);
assert.equal(decodeFrame(good.subarray(0, 10)), null);

// ACK payload: sequence number then note.
const ack = parseAck(new Uint8Array([0x2a, 0, 0, 0, ...new TextEncoder().encode("elevated")]));
assert.deepEqual(ack, { seq: 42, note: "elevated" });
assert.equal(parseAck(new Uint8Array([1, 2])), null);

assert.equal(MAX_PAYLOAD, 65536);
console.log("hostagent frame codec ok");
