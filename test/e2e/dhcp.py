#!/usr/bin/env python3
"""Decode DHCPv4 evidence from a classic pcap capture."""

import argparse
import json
import shutil
import struct
import sys
import tempfile


PCAP_MAGICS = {
    b"\xa1\xb2\xc3\xd4": (">", 1_000_000),
    b"\xd4\xc3\xb2\xa1": ("<", 1_000_000),
    b"\xa1\xb2\x3c\x4d": (">", 1_000_000_000),
    b"\x4d\x3c\xb2\xa1": ("<", 1_000_000_000),
}
PCAPNG_MAGIC = b"\x0a\x0d\x0d\x0a"

LINKTYPE_ETHERNET = 1
LINKTYPE_LINUX_SLL = 113
LINKTYPE_LINUX_SLL2 = 276
SUPPORTED_LINK_TYPES = {
    LINKTYPE_ETHERNET: "Ethernet",
    LINKTYPE_LINUX_SLL: "Linux cooked v1",
    LINKTYPE_LINUX_SLL2: "Linux cooked v2",
}
VLAN_ETHERTYPES = {0x8100, 0x88A8, 0x9100, 0x9200, 0x9300}
IPV4_ETHERTYPE = 0x0800
MAX_VLAN_TAGS = 8
MAX_RECORD_BYTES = 16 * 1024 * 1024
DHCP_COOKIE = b"\x63\x82\x53\x63"
DHCP_MESSAGES = {
    1: "DISCOVER",
    2: "OFFER",
    3: "REQUEST",
    4: "DECLINE",
    5: "ACK",
    6: "NAK",
    7: "RELEASE",
    8: "INFORM",
}
CLIENT_MESSAGES = {1, 3, 4, 7, 8}


class CaptureError(Exception):
    """The capture cannot be used as DHCP evidence."""


def _fail(where, message):
    raise CaptureError(f"{where}: {message}")


def _u16(data, offset):
    return struct.unpack_from("!H", data, offset)[0]


def _ipv4(data):
    return ".".join(str(octet) for octet in data)


def _link_payload(frame, link_type, where):
    if link_type == LINKTYPE_ETHERNET:
        if len(frame) < 14:
            _fail(where, "truncated Ethernet header")
        protocol = _u16(frame, 12)
        offset = 14
    elif link_type == LINKTYPE_LINUX_SLL:
        if len(frame) < 16:
            _fail(where, "truncated Linux cooked v1 header")
        if _u16(frame, 4) > 8:
            _fail(where, "invalid Linux cooked v1 address length")
        protocol = _u16(frame, 14)
        offset = 16
    elif link_type == LINKTYPE_LINUX_SLL2:
        if len(frame) < 20:
            _fail(where, "truncated Linux cooked v2 header")
        if _u16(frame, 2) != 0:
            _fail(where, "nonzero Linux cooked v2 reserved field")
        if frame[11] > 8:
            _fail(where, "invalid Linux cooked v2 address length")
        protocol = _u16(frame, 0)
        offset = 20
    else:
        _fail(where, f"unsupported link type {link_type}")

    vlan_count = 0
    while protocol in VLAN_ETHERTYPES:
        vlan_count += 1
        if vlan_count > MAX_VLAN_TAGS:
            _fail(where, f"more than {MAX_VLAN_TAGS} nested VLAN tags")
        if len(frame) < offset + 4:
            _fail(where, "truncated VLAN header")
        protocol = _u16(frame, offset + 2)
        offset += 4

    return protocol, frame[offset:]


def _parse_option_area(data, where):
    options = {}
    offset = 0
    while offset < len(data):
        code = data[offset]
        offset += 1
        if code == 0:
            continue
        if code == 255:
            return options
        if offset == len(data):
            _fail(where, f"option {code} is missing its length")
        length = data[offset]
        offset += 1
        end = offset + length
        if end > len(data):
            _fail(where, f"option {code} overruns its option area")
        options.setdefault(code, []).append(data[offset:end])
        offset = end
    _fail(where, "DHCP options have no end marker")


def _merge_options(options, extra):
    for code, values in extra.items():
        options.setdefault(code, []).extend(values)


def _option_value(options, code, name, where, expected_length=None):
    values = options.get(code)
    if values is None:
        return None
    value = b"".join(values)
    if expected_length is not None and len(value) != expected_length:
        _fail(
            where,
            f"{name} option has length {len(value)}, expected {expected_length}",
        )
    return value


def _option_ips(options, code, name, where):
    value = _option_value(options, code, name, where)
    if value is None:
        return []
    if not value or len(value) % 4 != 0:
        _fail(where, f"{name} option length must be a nonzero multiple of 4")
    return [_ipv4(value[offset : offset + 4]) for offset in range(0, len(value), 4)]


def _decode_dhcp(data, timestamp, ip_src, ip_dst, where):
    if len(data) < 236:
        _fail(where, "UDP ports identify DHCP but the BOOTP payload is shorter than 236 bytes")
    if len(data) < 240:
        return None
    if data[236:240] != DHCP_COOKIE:
        return None

    operation = data[0]
    hardware_type = data[1]
    hardware_length = data[2]
    if operation not in (1, 2):
        _fail(where, f"invalid BOOTP operation {operation}")
    if hardware_type != 1 or hardware_length != 6:
        _fail(
            where,
            f"unsupported BOOTP hardware address type/length {hardware_type}/{hardware_length}",
        )

    primary_options = _parse_option_area(data[240:], f"{where} primary options")
    overload = _option_value(
        primary_options,
        52,
        "option overload",
        where,
        expected_length=1,
    )
    options = {code: list(values) for code, values in primary_options.items()}
    if overload is not None:
        overload_flags = overload[0]
        if overload_flags not in (1, 2, 3):
            _fail(where, f"invalid option overload value {overload_flags}")
        if overload_flags & 1:
            _merge_options(
                options,
                _parse_option_area(data[108:236], f"{where} overloaded file field"),
            )
        if overload_flags & 2:
            _merge_options(
                options,
                _parse_option_area(data[44:108], f"{where} overloaded sname field"),
            )

    message_value = _option_value(
        options,
        53,
        "DHCP message type",
        where,
        expected_length=1,
    )
    if message_value is None:
        _fail(where, "DHCP message type option is missing")
    message_number = message_value[0]
    message = DHCP_MESSAGES.get(message_number)
    if message is None:
        _fail(where, f"unsupported DHCP message type {message_number}")

    expected_operation = 1 if message_number in CLIENT_MESSAGES else 2
    if operation != expected_operation:
        _fail(
            where,
            f"{message} uses BOOTP operation {operation}, expected {expected_operation}",
        )

    lease_value = _option_value(
        options,
        51,
        "lease time",
        where,
        expected_length=4,
    )
    subnet_value = _option_value(
        options,
        1,
        "subnet mask",
        where,
        expected_length=4,
    )
    server_value = _option_value(
        options,
        54,
        "server identifier",
        where,
        expected_length=4,
    )

    flags = _u16(data, 10)
    return {
        "time": timestamp,
        "message": message,
        "mac": ":".join(f"{octet:02x}" for octet in data[28:34]),
        "xid": struct.unpack_from("!I", data, 4)[0],
        "ciaddr": _ipv4(data[12:16]),
        "yiaddr": _ipv4(data[16:20]),
        "ip_src": ip_src,
        "ip_dst": ip_dst,
        "broadcast": bool(flags & 0x8000),
        "lease_seconds": (
            struct.unpack("!I", lease_value)[0] if lease_value is not None else None
        ),
        "subnet": _ipv4(subnet_value) if subnet_value is not None else None,
        "routers": _option_ips(options, 3, "router", where),
        "dns": _option_ips(options, 6, "DNS server", where),
        "server_id": _ipv4(server_value) if server_value is not None else None,
    }


def _decode_ipv4(packet, timestamp, where):
    if len(packet) < 20:
        _fail(where, "truncated IPv4 header")

    version = packet[0] >> 4
    header_length = (packet[0] & 0x0F) * 4
    if version != 4:
        _fail(where, f"EtherType says IPv4 but header version is {version}")
    if header_length < 20:
        _fail(where, f"invalid IPv4 header length {header_length}")
    if len(packet) < header_length:
        _fail(where, "truncated IPv4 options")

    total_length = _u16(packet, 2)
    if total_length < header_length:
        _fail(where, f"IPv4 total length {total_length} is shorter than its header")
    if total_length > len(packet):
        _fail(
            where,
            f"IPv4 packet is truncated: header says {total_length} bytes, capture has {len(packet)}",
        )
    packet = packet[:total_length]

    fragment = _u16(packet, 6)
    if fragment & 0x8000:
        _fail(where, "IPv4 reserved fragment flag is set")
    fragment_offset = fragment & 0x1FFF
    more_fragments = bool(fragment & 0x2000)

    if packet[9] != 17:
        return None
    if fragment_offset:
        return None

    udp = packet[header_length:]
    if len(udp) < 8:
        _fail(where, "truncated UDP header")
    source_port, destination_port, udp_length, _checksum = struct.unpack_from(
        "!HHHH", udp, 0
    )
    is_dhcp = (
        (source_port == 68 and destination_port == 67)
        or (source_port == 67 and destination_port in (67, 68))
    )
    if not is_dhcp:
        return None
    if more_fragments:
        _fail(where, "fragmented DHCP packets are unsupported")
    if udp_length < 8:
        _fail(where, f"invalid UDP length {udp_length}")
    if udp_length != len(udp):
        _fail(
            where,
            f"UDP length {udp_length} does not match IPv4 payload length {len(udp)}",
        )

    return _decode_dhcp(
        udp[8:],
        timestamp,
        _ipv4(packet[12:16]),
        _ipv4(packet[16:20]),
        where,
    )


def _decode_frame(frame, link_type, timestamp, where):
    protocol, payload = _link_payload(frame, link_type, where)
    if protocol != IPV4_ETHERTYPE:
        return None
    return _decode_ipv4(payload, timestamp, where)


def _partial_global_header_is_possible(header):
    if len(header) >= 4:
        return header[:4] in PCAP_MAGICS
    return any(magic.startswith(header) for magic in PCAP_MAGICS)


def _read_global_header(capture, allow_incomplete):
    header = capture.read(24)
    if len(header) != 24:
        if allow_incomplete and _partial_global_header_is_possible(header):
            return None
        _fail("global header", f"truncated: found {len(header)} of 24 bytes")

    magic = header[:4]
    if magic == PCAPNG_MAGIC:
        _fail("global header", "pcapng is unsupported; expected classic pcap")
    byte_order_and_resolution = PCAP_MAGICS.get(magic)
    if byte_order_and_resolution is None:
        _fail("global header", f"unsupported pcap magic {magic.hex()}")
    byte_order, timestamp_resolution = byte_order_and_resolution

    major, minor, _zone, _sigfigs, snaplen, link_type = struct.unpack(
        byte_order + "HHiIII", header[4:]
    )
    if (major, minor) != (2, 4):
        _fail("global header", f"unsupported pcap version {major}.{minor}")
    if snaplen == 0:
        _fail("global header", "snapshot length is zero")
    if link_type not in SUPPORTED_LINK_TYPES:
        supported = ", ".join(
            f"{name} ({number})" for number, name in SUPPORTED_LINK_TYPES.items()
        )
        _fail(
            "global header",
            f"unsupported link type {link_type}; supported: {supported}",
        )

    return byte_order, timestamp_resolution, snaplen, link_type


def _decode_capture(capture, output, allow_incomplete):
    global_header = _read_global_header(capture, allow_incomplete)
    if global_header is None:
        return
    byte_order, timestamp_resolution, snaplen, link_type = global_header

    record_number = 0
    while True:
        record_header = capture.read(16)
        if not record_header:
            return
        record_number += 1
        where = f"record {record_number}"
        if len(record_header) != 16:
            if allow_incomplete:
                return
            _fail(where, f"truncated record header: found {len(record_header)} of 16 bytes")

        seconds, fraction, included_length, original_length = struct.unpack(
            byte_order + "IIII", record_header
        )
        if fraction >= timestamp_resolution:
            _fail(
                where,
                f"timestamp fraction {fraction} exceeds the capture resolution",
            )
        if included_length > MAX_RECORD_BYTES:
            _fail(
                where,
                f"included length {included_length} exceeds the {MAX_RECORD_BYTES}-byte safety limit",
            )
        if included_length > snaplen:
            _fail(
                where,
                f"included length {included_length} exceeds snapshot length {snaplen}",
            )
        if included_length > original_length:
            _fail(
                where,
                f"included length {included_length} exceeds original length {original_length}",
            )
        if included_length < original_length:
            _fail(
                where,
                f"packet was truncated by capture: included {included_length}, original {original_length}",
            )

        frame = capture.read(included_length)
        if len(frame) != included_length:
            if allow_incomplete:
                return
            _fail(
                where,
                f"truncated packet data: found {len(frame)} of {included_length} bytes",
            )

        timestamp = seconds + fraction / timestamp_resolution
        event = _decode_frame(frame, link_type, timestamp, where)
        if event is not None:
            output.write(
                json.dumps(event, ensure_ascii=True, separators=(",", ":")) + "\n"
            )


def _parse_args(argv):
    parser = argparse.ArgumentParser(
        description="Decode DHCPv4 evidence from a classic pcap file as JSONL."
    )
    parser.add_argument(
        "--allow-incomplete",
        action="store_true",
        help="ignore only an unfinished final live-capture header or record",
    )
    parser.add_argument("pcap_file", metavar="pcap-file")
    return parser.parse_args(argv)


def main(argv=None):
    args = _parse_args(argv)
    try:
        with open(args.pcap_file, "rb") as capture:
            with tempfile.SpooledTemporaryFile(
                max_size=1024 * 1024,
                mode="w+",
                encoding="utf-8",
                newline="\n",
            ) as pending:
                _decode_capture(capture, pending, args.allow_incomplete)
                pending.seek(0)
                shutil.copyfileobj(pending, sys.stdout)
    except CaptureError as error:
        print(f"dhcp.py: error: {error}", file=sys.stderr)
        return 1
    except OSError as error:
        print(f"dhcp.py: error: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
