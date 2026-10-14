// Writes testdata/tcd-wire-vectors.txt from the NATIVE struct memory of
// the unpatched STCPacket (memcpy), so the vectors are the ground truth
// of the raw wire format on the build machine. The v2 frame is built here
// with its own CRC code, independent of the patched tcd.
//
// Build: g++ -std=c++17 -I<unpatched tcd> genvectors.cpp -o genvectors

#include <cstdio>
#include <cstring>
#include <iostream>

#include "TCPacketDef.h"
#include "vectors.h"

static uint32_t crc32(const uint8_t *p, std::size_t n)
{
	uint32_t c = 0xFFFFFFFFu;
	for (std::size_t i = 0; i < n; i++)
	{
		c ^= p[i];
		for (int k = 0; k < 8; k++)
			c = (c >> 1) ^ (0xEDB88320u & (0u - (c & 1u)));
	}
	return ~c;
}

// The reply rule of the patched tcd, written again here: the requested
// fields stay, the others are zero. 0 or no known bit = all; the c2_1600
// bit selects the m17 field.
static uint8_t normalize(uint8_t m)
{
	if (m & 0x08) m |= 0x10;
	m &= 0x76;
	return m ? m : 0x76;
}

static void emit(const char *name, const STCPacket &p, uint8_t outputs)
{
	uint8_t raw[sizeof(STCPacket)];
	std::memcpy(raw, &p, sizeof(raw));
	uint8_t v2[8 + sizeof(STCPacket) + 4];
	std::memcpy(v2, "TCD2", 4);
	v2[4] = 1;
	v2[5] = outputs;
	v2[6] = uint8_t(sizeof(STCPacket) & 0xFF);
	v2[7] = uint8_t(sizeof(STCPacket) >> 8);
	std::memcpy(v2 + 8, raw, sizeof(raw));
	uint32_t c = crc32(v2 + 4, 4 + sizeof(raw));
	for (int i = 0; i < 4; i++)
		v2[8 + sizeof(raw) + i] = uint8_t(c >> (8 * i));

	const uint8_t want = normalize(outputs);
	STCPacket q = p;
	if (! (want & 0x02)) std::memset(q.dstar, 0, 9);
	if (! (want & 0x04)) std::memset(q.dmr, 0, 9);
	if (! (want & 0x10)) std::memset(q.m17, 0, 16);
	if (! (want & 0x20)) std::memset(q.p25, 0, 11);
	if (! (want & 0x40)) std::memset(q.usrp, 0, sizeof(q.usrp));
	uint8_t reply[sizeof(STCPacket)];
	std::memcpy(reply, &q, sizeof(reply));
	char ob[8], rb[8];
	snprintf(ob, sizeof(ob), "0x%02x", unsigned(outputs));
	snprintf(rb, sizeof(rb), "0x%02x", unsigned(want));

	std::cout << "name=" << name << " outputs=" << ob << " reply_outputs=" << rb << " seq=" << p.sequence << " module=" << p.module
	          << " last=" << (p.is_last ? 1 : 0) << " sid=" << p.streamid
	          << " codec=" << unsigned(uint8_t(p.codec_in))
	          << " dstar=" << ToHex(p.dstar, 9) << " dmr=" << ToHex(p.dmr, 9)
	          << " m17=" << ToHex(p.m17, 16) << " p25=" << ToHex(p.p25, 11) << " usrp=";
	for (int i = 0; i < 160; i++)
		std::cout << (i ? "," : "") << p.usrp[i];
	std::cout << " raw=" << ToHex(raw, sizeof(raw)) << " v2=" << ToHex(v2, sizeof(v2))
	          << " reply=" << ToHex(reply, sizeof(reply)) << "\n";
}

int main()
{
	std::cout << "# tcd link test vectors, raw (376 bytes) and v2 (388 bytes).\n"
	          << "# outputs: the requested-outputs byte of the v2 frame. reply and\n"
	          << "# reply_outputs: the raw reply and outputs byte that tcd sends back\n"
	          << "# when every requested field is the same as in the request.\n"
	          << "# Made by tests/tcd-wire/genvectors.cpp from the native struct memory\n"
	          << "# of the unpatched nostar tcd STCPacket (g++, little-endian Linux).\n"
	          << "# Read by tests/tcd-wire/codec_test.cpp and internal/backend/nostar.\n";
	STCPacket p;

	std::memset(&p, 0, sizeof(p));
	p.sequence = 0; p.module = 'A'; p.is_last = false; p.streamid = 0x1234; p.codec_in = ECodecType::usrp;
	for (int i = 0; i < 160; i++) p.usrp[i] = int16_t(i * 200 - 16000);
	emit("usrp_ramp", p, 0x00);

	std::memset(&p, 0, sizeof(p));
	p.sequence = 0x01020304; p.module = 'B'; p.is_last = true; p.streamid = 0xBEEF; p.codec_in = ECodecType::dmr;
	for (int i = 0; i < 9; i++) { p.dstar[i] = uint8_t(0x10 + i); p.dmr[i] = uint8_t(0xA0 + i); }
	for (int i = 0; i < 16; i++) p.m17[i] = uint8_t(0x30 + i);
	for (int i = 0; i < 11; i++) p.p25[i] = uint8_t(0x40 + i);
	for (int i = 0; i < 160; i++) p.usrp[i] = int16_t(-i * 7);
	emit("dmr_all_fields", p, 0x42); // dstar + usrp

	std::memset(&p, 0, sizeof(p));
	p.module = 'A'; p.codec_in = ECodecType::ping;
	emit("ping", p, 0x00);

	std::memset(&p, 0, sizeof(p));
	p.sequence = 0xFFFFFFFFu; p.module = 'Z'; p.is_last = false; p.streamid = 0xFFFF; p.codec_in = ECodecType::c2_3200;
	for (int i = 0; i < 16; i++) p.m17[i] = uint8_t(0xF0 + i);
	for (int i = 0; i < 160; i++) p.usrp[i] = (i % 2) ? int16_t(32767) : int16_t(-32768);
	emit("c2_extremes", p, 0x0c); // dmr + c2_1600 (= m17)
	return 0;
}
