# SPDX-License-Identifier: MIT
import re
import struct
import unittest
from pathlib import Path
import acceptance as a


class WireHarnessTests(unittest.TestCase):
    def test_multiple_records_and_unknown_type(self):
        raw = a.pack(1, b'{}') + a.pack(90, b'ignored') + a.pack(2, b'abc')
        self.assertEqual(a.records(raw), [(1, b'{}'), (90, b'ignored'), (2, b'abc')])

    def test_rejects_truncated_framing(self):
        for raw in (b'x', b'\x02\0\0\0', a.pack(3, b'hello')[:-1]):
            with self.subTest(raw=raw), self.assertRaises(AssertionError):
                a.records(raw)

    def test_checks_length_before_reading_payload(self):
        with self.assertRaises(AssertionError):
            a.records(struct.pack('>BI', 2, a.MAX_PAYLOAD + 1))

    def test_fixture_and_embedded_header_match(self):
        root = Path(__file__).resolve().parents[1]
        fixture = (root / 'tests/decoder-selftest.h264').read_bytes()
        array = (root / 'decoder_fixture.h').read_text().split('{', 1)[1]
        header = bytes(int(x, 16) for x in re.findall(r'0x([0-9a-f]{2})', array))
        self.assertEqual(fixture, header)
        self.assertEqual([n[0] & 31 for n in a.nals(fixture)], [7, 8, 5])
        self.assertNotEqual(a.slice_type(a.nals(fixture)[-1]), 1)

    def test_slice_types(self):
        self.assertEqual(a.slice_type(b'\x41\xa0'), 1)
        self.assertEqual(a.slice_type(b'\x41\xe0'), 0)

    def test_annex_b_prefixes(self):
        self.assertEqual(a.nals(b'\0\0\1\x67\x01\0\0\0\1\x68\x02'), [b'\x67\x01', b'\x68\x02'])
        with self.assertRaises(AssertionError):
            a.nals(b'no start code')

    def test_display_device_suffix(self):
        prefix = r'\\.\DISPLAY'
        self.assertEqual(prefix, '\\\\.\\DISPLAY')
        self.assertEqual(int((prefix + '12')[len(prefix):]), 12)


if __name__ == '__main__':
    unittest.main()
