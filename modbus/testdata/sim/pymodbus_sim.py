#!/usr/bin/env python3
"""pymodbus_sim.py — the FOREIGN Modbus TCP implementation modbus/foreign_test.go
talks to: a pymodbus server whose datastore is seeded with known values, so the
driver's decoder, block planner, write coalescing and exception handling are
checked against somebody else's encoder rather than our own slave.

    python3 pymodbus_sim.py [--host 127.0.0.1] [--port 5020] [--latency-ms N]

Prints one line, "listening on HOST:PORT", once it accepts connections.
Requires pymodbus 3.15.x (SimData/SimDevice API — see the pin in ci.yml).

REGISTER MAP — foreign_test.go asserts this table byte for byte; change both.
Four unit ids, identical maps, differing only in how multi-register values and
bytes are laid out (the driver's per-source wordorder/byteorder):

    unit 1: wordorder big,    byteorder big     (the Modbus default)
    unit 2: wordorder little, byteorder big     (Anybus/Omron style)
    unit 3: wordorder big,    byteorder little
    unit 4: wordorder little, byteorder little

Addresses are 0-based PDU addresses. Words shown are the big-endian layout;
the other units permute them (unit 2 reverses the words of each value, unit 3
swaps the bytes within every register, unit 4 does both).

  HOLDING (FC3, writable)          | INPUT (FC4, read-only)
  addr  format   value    words    | addr  format   value       words
  0     int16    -1234    FB2E     | 0     int16    3210        0C8A
  1     uint16   54321    D431     | 1     uint16   65535       FFFF
  2-3   int32    -123456789        | 2-3   int32    2147483647  7FFF FFFF
                 F8A4 32EB         |
  4-5   uint32   3000000000        | 4-5   uint32   4000000000  EE6B 2800
                 B2D0 5E00         |
  6-7   float32  -1.5     BFC0 0000| 6-7   float32  3.25        4050 0000
  8-11  float64  6.02214076e23     | 8-11  float64  -2.5        C004 0000 0000 0000
                 44DF E185 CA57 C517
  12    bit:N    0xA5C3   A5C3     | 12    bit:N    0x5A3C      5A3C
  13    bool     true     0001     | 13    bool     false       0000
  14    int16    raw 250  00FA     | 14    uint16   raw 7       0007
        (scale 0.5, offset -5 → 120.0)     (scale 0.25, offset 1 → 2.75)
  200-215  scratch, zero, for the write round-trip test
  1000+    NOT IMPLEMENTED → exception 0x02 (illegal data address)

  COILS (FC1, writable)            | DISCRETE INPUTS (FC2)
  0-15     bit i of 0xA5C3         | 0-15   bit i of 0x5A3C
  100-115  scratch, false          | 1000+  NOT IMPLEMENTED → exception 0x02
  1000+    NOT IMPLEMENTED → exception 0x02
"""

import argparse
import asyncio
import struct

from pymodbus.server import ModbusTcpServer
from pymodbus.simulator import DataType, SimData, SimDevice

# (struct format, engineering value) per register-table cell, in address order.
# struct is the foreign encoder: it renders each value big-endian and the unit
# permutation below is the only thing we do to its bytes.
HOLDING = [("h", -1234), ("H", 54321), ("i", -123456789), ("I", 3000000000),
           ("f", -1.5), ("d", 6.02214076e23), ("H", 0xA5C3), ("H", 1), ("h", 250)]
INPUT = [("h", 3210), ("H", 65535), ("i", 2147483647), ("I", 4000000000),
         ("f", 3.25), ("d", -2.5), ("H", 0x5A3C), ("H", 0), ("H", 7)]
COILS = 0xA5C3
DISCRETE = 0x5A3C
SCRATCH_REG, SCRATCH_COIL, SCRATCH_LEN = 200, 100, 16

# Unit id → (word order little, byte order little).
UNITS = {1: (False, False), 2: (True, False), 3: (False, True), 4: (True, True)}


def words(cells, word_little, byte_little):
    """Render the cells as one contiguous register block in the unit's layout."""
    out = []
    for fmt, value in cells:
        raw = struct.pack(">" + fmt, value)
        regs = [raw[i] << 8 | raw[i + 1] for i in range(0, len(raw), 2)]
        if word_little:
            regs.reverse()
        if byte_little:
            regs = [(r & 0xFF) << 8 | r >> 8 for r in regs]
        out.extend(regs)
    return out


def bits(pattern):
    return [bool(pattern >> i & 1) for i in range(16)]


def device(unit, word_little, byte_little, action):
    coils = [SimData(0, values=bits(COILS), datatype=DataType.BITS),
             SimData(SCRATCH_COIL, values=[False] * SCRATCH_LEN, datatype=DataType.BITS)]
    discrete = [SimData(0, values=bits(DISCRETE), datatype=DataType.BITS)]
    holding = [SimData(0, values=words(HOLDING, word_little, byte_little), datatype=DataType.REGISTERS),
               SimData(SCRATCH_REG, values=[0] * SCRATCH_LEN, datatype=DataType.REGISTERS)]
    inputs = [SimData(0, values=words(INPUT, word_little, byte_little), datatype=DataType.REGISTERS)]
    # The tuple form gives four distinct tables (like a real device) and leaves
    # every address outside these SimData ranges unimplemented: pymodbus answers
    # exception 0x02 there, which is what the driver's exception test needs.
    return SimDevice(id=unit, simdata=(coils, discrete, holding, inputs), action=action)


async def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n", 1)[0])
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=5020)
    ap.add_argument("--latency-ms", type=int, default=0,
                    help="delay every request by this much (drives the driver's timeout path)")
    args = ap.parse_args()

    action = None
    if args.latency_ms > 0:
        # SimDevice.action runs inside every read/write before the datastore
        # answers; sleeping here is the per-request latency hook.
        async def action(*_):
            await asyncio.sleep(args.latency_ms / 1000)
            return None

    devices = [device(u, wl, bl, action) for u, (wl, bl) in UNITS.items()]
    server = ModbusTcpServer(devices, address=(args.host, args.port))
    await server.serve_forever(background=True)
    print(f"listening on {args.host}:{args.port}", flush=True)
    await server.serving


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        pass
