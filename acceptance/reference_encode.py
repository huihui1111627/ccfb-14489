#!/usr/bin/env python3
"""Independent reference encoder for protobuf wire format.

This is deliberately a second implementation, written from the wire-format
specification, to produce expectations for the Go acceptance tests. It does
not import or call the Go code under test.
"""

def uvarint(n: int) -> bytes:
    out = bytearray()
    while True:
        b = n & 0x7F
        n >>= 7
        if n:
            out.append(b | 0x80)
        else:
            out.append(b)
            return bytes(out)


def tag(field: int, wire: int) -> bytes:
    return uvarint((field << 3) | wire)


def ld(field: int, payload: bytes) -> bytes:
    return tag(field, 2) + uvarint(len(payload)) + payload


def s64(n: int) -> bytes:
    return (n & ((1 << 64) - 1)).to_bytes(8, "little")


def s32(n: int) -> bytes:
    return (n & ((1 << 32) - 1)).to_bytes(4, "little")


def dbl(x: float) -> bytes:
    import struct
    return struct.pack("<d", x)


def flt(x: float) -> bytes:
    import struct
    return struct.pack("<f", x)


def zigzag(n: int, bits: int) -> bytes:
    return uvarint(((n << 1) ^ (n >> (bits - 1))) & ((1 << bits) - 1))


def svarint(n: int) -> bytes:
    """int32/int64: negative values are sign-extended to 64 bits."""
    return uvarint(n & ((1 << 64) - 1))


def field(field_no: int, wire: int, payload: bytes) -> bytes:
    return tag(field_no, wire) + payload


def main() -> None:
    vectors = {}

    # Book{name:"dune", title:"Dune", isbn:1234567890123, authors:["a"],
    #      genre:FICTION}
    book = (
        ld(1, b"dune")
        + ld(2, b"Dune")
        + field(3, 0, svarint(1234567890123))
        + ld(4, b"a")
        + field(5, 0, uvarint(1))
    )
    vectors["book"] = book.hex()

    # ScalarBag covering every wire type.
    bag = b""
    bag += field(1, 1, dbl(1.5))                       # double
    bag += field(2, 5, flt(-2.25))                     # float
    bag += field(3, 0, svarint(-1))                    # int32 -1 -> 10 bytes
    bag += field(4, 0, svarint(-2))                    # int64 -2
    bag += field(5, 0, uvarint(300))                   # uint32
    bag += field(6, 0, uvarint((1 << 64) - 1))         # uint64 max
    bag += field(7, 0, zigzag(-7, 32))                 # sint32
    bag += field(8, 0, zigzag(-8, 64))                 # sint64
    bag += field(9, 5, s32(0xDEADBEEF))                # fixed32
    bag += field(10, 1, s64(0x0102030405060708))       # fixed64
    bag += field(11, 5, s32(-9))                       # sfixed32
    bag += field(12, 1, s64(-10))                      # sfixed64
    bag += field(13, 0, b"\x01")                       # bool
    bag += ld(14, "héllo".encode())                    # string
    bag += ld(15, bytes.fromhex("00ff10"))             # bytes
    bag += field(16, 0, uvarint(2))                    # enum
    bag += ld(17, uvarint(1) + svarint(-1) + uvarint(2))  # packed int32
    bag += field(18, 0, uvarint(5)) + field(18, 0, uvarint(6))  # unpacked
    vectors["scalarbag"] = bag.hex()

    # Timestamp and Duration payloads.
    vectors["timestamp_120ms"] = (
        field(1, 0, uvarint(1700000000)) + field(2, 0, uvarint(120000000))
    ).hex()
    vectors["duration_1_5s"] = (
        field(1, 0, uvarint(1)) + field(2, 0, uvarint(500000000))
    ).hex()

    # Map<string, Shelf> entry for {"top": {name:"t", topic:"sci"}}.
    entry = ld(1, b"top") + ld(2, ld(1, b"t") + ld(2, b"sci"))
    vectors["shelf_entry"] = entry.hex()

    for k, v in vectors.items():
        print(k, v)


if __name__ == "__main__":
    main()
