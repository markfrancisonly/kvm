// Wire format shared with the device (internal/hostagent) and the host agent:
// "JKVM" | version u8 = 1 | type u8 | flags u8 | reserved u8 | seq u32 | length u32 | payload | crc32
// Little-endian; the CRC-32 (IEEE) covers everything before it. The browser
// only ever sends blob frames, which the device relays to the agent.

export const FRAME_VERSION = 1;
export const HEADER_SIZE = 16;
export const MAX_PAYLOAD = 64 * 1024;

export const FrameType = {
  Hello: 0x01,
  Ack: 0x02,
  Nack: 0x03,
  Ping: 0x04,
  Pong: 0x05,
  Bye: 0x06,
  Text: 0x10,
  BlobBegin: 0x20,
  BlobData: 0x21,
  BlobEnd: 0x22,
  BlobCancel: 0x23,
  Status: 0x30,
} as const;

export interface Frame {
  type: number;
  flags: number;
  seq: number;
  payload: Uint8Array;
}

const MAGIC = [0x4a, 0x4b, 0x56, 0x4d];

const crcTable = (() => {
  const t = new Uint32Array(256);
  for (let i = 0; i < 256; i++) {
    let c = i;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[i] = c >>> 0;
  }
  return t;
})();

export function crc32(data: Uint8Array, end = data.length): number {
  let crc = 0xffffffff;
  for (let i = 0; i < end; i++) crc = crcTable[(crc ^ data[i]) & 0xff] ^ (crc >>> 8);
  return (crc ^ 0xffffffff) >>> 0;
}

export function encodeFrame(f: Omit<Frame, "flags"> & { flags?: number }): Uint8Array<ArrayBuffer> {
  const n = HEADER_SIZE + f.payload.length + 4;
  const b = new Uint8Array(n);
  const v = new DataView(b.buffer);
  b.set(MAGIC, 0);
  b[4] = FRAME_VERSION;
  b[5] = f.type;
  b[6] = f.flags ?? 0;
  v.setUint32(8, f.seq >>> 0, true);
  v.setUint32(12, f.payload.length, true);
  b.set(f.payload, HEADER_SIZE);
  v.setUint32(n - 4, crc32(b, n - 4), true);
  return b;
}

// Parses one frame that fills b exactly, or returns null.
export function decodeFrame(b: Uint8Array): Frame | null {
  if (b.length < HEADER_SIZE + 4) return null;
  for (let i = 0; i < 4; i++) if (b[i] !== MAGIC[i]) return null;
  if (b[4] !== FRAME_VERSION) return null;
  const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
  const length = v.getUint32(12, true);
  if (length > MAX_PAYLOAD || HEADER_SIZE + length + 4 !== b.length) return null;
  if (crc32(b, b.length - 4) !== v.getUint32(b.length - 4, true)) return null;
  return {
    type: b[5],
    flags: b[6],
    seq: v.getUint32(8, true),
    payload: b.slice(HEADER_SIZE, HEADER_SIZE + length),
  };
}

// ACK and NACK payloads: the acknowledged sequence number and a note.
export function parseAck(payload: Uint8Array): { seq: number; note: string } | null {
  if (payload.length < 4) return null;
  const seq = new DataView(payload.buffer, payload.byteOffset, payload.byteLength).getUint32(
    0,
    true,
  );
  return { seq, note: new TextDecoder().decode(payload.subarray(4)) };
}

// BLOB_DATA payload: blob id, byte offset, bytes.
export function blobDataPayload(
  id: number,
  offset: number,
  bytes: Uint8Array,
): Uint8Array<ArrayBuffer> {
  const p = new Uint8Array(8 + bytes.length);
  const v = new DataView(p.buffer);
  v.setUint32(0, id >>> 0, true);
  v.setUint32(4, offset >>> 0, true);
  p.set(bytes, 8);
  return p;
}

export function blobIdPayload(id: number): Uint8Array<ArrayBuffer> {
  const p = new Uint8Array(4);
  new DataView(p.buffer).setUint32(0, id >>> 0, true);
  return p;
}
