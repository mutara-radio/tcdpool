// Unit test for the patched tcd link code: TCPacketCodec (explicit
// encoding) and TCFraming (v2 frames), against the shared vectors.
//
// Build: see the tcd-test stage in the Dockerfile.
// Run:   ./codec_test testdata/tcd-wire-vectors.txt

#include <cstring>
#include <iostream>

#include "TCPacketCodec.h"
#include "TCFraming.h"
#include "TranscoderPacket.h"
#include "vectors.h"

static int failures = 0;
static int checks = 0;

#define CHECK(cond, what)                                                        \
	do {                                                                         \
		checks++;                                                                \
		if (! (cond)) {                                                          \
			failures++;                                                          \
			std::cerr << "FAIL " << __LINE__ << ": " << what << std::endl;       \
		}                                                                        \
	} while (0)

static STCPacket FromVector(const SVector &v)
{
	STCPacket p;
	std::memset(&p, 0, sizeof(p));
	p.sequence = uint32_t(std::strtoul(v.f.at("seq").c_str(), nullptr, 10));
	p.module = v.f.at("module")[0];
	p.is_last = v.f.at("last") == "1";
	p.streamid = uint16_t(std::strtoul(v.f.at("sid").c_str(), nullptr, 10));
	p.codec_in = ECodecType(std::strtoul(v.f.at("codec").c_str(), nullptr, 10));
	auto cp = [&](const char *k, uint8_t *dst, std::size_t n) {
		auto b = FromHex(v.f.at(k));
		std::memcpy(dst, b.data(), std::min(n, b.size()));
	};
	cp("dstar", p.dstar, 9);
	cp("dmr", p.dmr, 9);
	cp("m17", p.m17, 16);
	cp("p25", p.p25, 11);
	std::stringstream ss(v.f.at("usrp"));
	std::string tok;
	int i = 0;
	while (std::getline(ss, tok, ',') && i < 160)
		p.usrp[i++] = int16_t(std::strtol(tok.c_str(), nullptr, 10));
	return p;
}

static bool SamePacket(const STCPacket &a, const STCPacket &b)
{
	return a.sequence == b.sequence && a.module == b.module && a.is_last == b.is_last &&
	       a.streamid == b.streamid && a.codec_in == b.codec_in &&
	       ! std::memcmp(a.dstar, b.dstar, 9) && ! std::memcmp(a.dmr, b.dmr, 9) &&
	       ! std::memcmp(a.m17, b.m17, 16) && ! std::memcmp(a.p25, b.p25, 11) &&
	       ! std::memcmp(a.usrp, b.usrp, sizeof(a.usrp));
}

// Feed bytes to a reader and collect the packets and the bad count.
struct SFeed
{
	std::vector<STCPacket> packets;
	std::vector<uint8_t> outputs;
	unsigned bad = 0;
	bool tooMany = false;
};

static void Drain(CV2Reader &r, SFeed &out)
{
	for (;;)
	{
		STCPacket p;
		const char *why = nullptr;
		uint8_t outputs = 0;
		auto st = r.Next(p, outputs, why);
		if (st == EV2Status::need_more)
			return;
		if (st == EV2Status::packet)
		{
			out.packets.push_back(p);
			out.outputs.push_back(outputs);
		}
		else if (st == EV2Status::bad)
			out.bad++;
		else
		{
			out.bad++;
			out.tooMany = true;
			return;
		}
	}
}

static SFeed Feed(const std::vector<uint8_t> &bytes, std::size_t chunk)
{
	CV2Reader r;
	SFeed out;
	for (std::size_t i = 0; i < bytes.size() && ! out.tooMany; i += chunk)
	{
		r.Append(bytes.data() + i, std::min(chunk, bytes.size() - i));
		Drain(r, out);
	}
	return out;
}

int main(int argc, char **argv)
{
	if (argc != 2)
	{
		std::cerr << "usage: codec_test <vectors file>" << std::endl;
		return 2;
	}
	auto vecs = ReadVectors(argv[1]);
	CHECK(vecs.size() >= 4, "at least 4 vectors");

	std::vector<uint8_t> stream;
	std::vector<STCPacket> expect;
	std::vector<uint8_t> expectOut;
	for (auto &v : vecs)
	{
		const std::string name = v.f["name"];
		const STCPacket p = FromVector(v);
		const auto raw = FromHex(v.f["raw"]);
		const auto v2 = FromHex(v.f["v2"]);
		CHECK(raw.size() == TCPacketWireSize, name << ": raw size");
		CHECK(v2.size() == kV2FrameSize, name << ": v2 size");

		// encode -> golden bytes
		uint8_t enc[TCPacketWireSize];
		EncodeTCPacket(p, enc);
		CHECK(! std::memcmp(enc, raw.data(), TCPacketWireSize), name << ": EncodeTCPacket != golden raw");

		// memcpy of the native struct (padding zeroed) on this build
		uint8_t native[sizeof(STCPacket)];
		std::memcpy(native, &p, sizeof(native));
		CHECK(! std::memcmp(native, raw.data(), TCPacketWireSize), name << ": native struct memory != golden raw");

		// decode(golden) -> fields
		STCPacket d;
		const char *why = DecodeTCPacket(raw.data(), d);
		CHECK(why == nullptr, name << ": decode failed: " << (why ? why : ""));
		if (p.codec_in != ECodecType::ping)
			CHECK(SamePacket(p, d), name << ": decode(golden) fields differ");

		// v2 frame with its requested-outputs byte -> golden bytes
		const uint8_t outputs = uint8_t(std::strtoul(v.f.at("outputs").c_str(), nullptr, 0));
		uint8_t frame[kV2FrameSize];
		EncodeV2Frame(p, outputs, frame);
		CHECK(! std::memcmp(frame, v2.data(), kV2FrameSize), name << ": EncodeV2Frame != golden v2");

		// the reply tcd sends for this request: unrequested fields zero
		if (p.codec_in != ECodecType::ping)
		{
			// fill every field as the encoders would, then take the reply
			CTranscoderPacket tp(p, outputs);
			tp.SetDStarData(p.dstar);
			tp.SetDMRData(p.dmr);
			tp.SetM17Data(p.m17);
			tp.SetP25Data(p.p25);
			tp.SetUSRPData(p.usrp);
			const STCPacket reply = tp.Reply();
			uint8_t rb[TCPacketWireSize];
			EncodeTCPacket(reply, rb);
			const auto golden = FromHex(v.f.at("reply"));
			CHECK(golden.size() == TCPacketWireSize && ! std::memcmp(rb, golden.data(), TCPacketWireSize), name << ": Reply() != golden reply");
			const uint8_t want = uint8_t(std::strtoul(v.f.at("reply_outputs").c_str(), nullptr, 0));
			CHECK(tp.Want() == want, name << ": reply outputs " << unsigned(tp.Want()) << ", want " << unsigned(want));
		}

		stream.insert(stream.end(), v2.begin(), v2.end());
		expect.push_back(p);
		expectOut.push_back(outputs);
	}

	// decode refuses bad values
	{
		auto raw = FromHex(vecs[0].f["raw"]);
		STCPacket d;
		raw[5] = 2;
		CHECK(DecodeTCPacket(raw.data(), d) != nullptr, "is_last=2 refused");
		raw[5] = 0;
		raw[8] = 0;
		CHECK(DecodeTCPacket(raw.data(), d) != nullptr, "codec_in=none refused");
		raw[8] = 7;
		CHECK(DecodeTCPacket(raw.data(), d) != nullptr, "codec_in=7 refused");
		raw[8] = 6;
		raw[374] = 0xAA;
		raw[375] = 0x55;
		CHECK(DecodeTCPacket(raw.data(), d) == nullptr, "padding is ignored on read");
	}

	// v2 reader: whole stream, in one piece and split in every chunk size
	for (std::size_t chunk : { std::size_t(4096), std::size_t(1), std::size_t(3), std::size_t(7), std::size_t(100), std::size_t(387) })
	{
		auto r = Feed(stream, chunk);
		CHECK(r.packets.size() == expect.size() && r.bad == 0, "split chunk=" << chunk << ": got " << r.packets.size() << " packets, " << r.bad << " bad");
		for (std::size_t i = 0; i < r.packets.size() && i < expect.size(); i++)
		{
			if (expect[i].codec_in != ECodecType::ping)
				CHECK(SamePacket(r.packets[i], expect[i]), "split chunk=" << chunk << ": packet " << i);
			CHECK(r.outputs[i] == expectOut[i], "split chunk=" << chunk << ": outputs of packet " << i);
		}
	}

	const auto good = FromHex(vecs[0].f["v2"]);
	auto cat = [](std::initializer_list<std::vector<uint8_t>> parts) {
		std::vector<uint8_t> out;
		for (auto &p : parts) out.insert(out.end(), p.begin(), p.end());
		return out;
	};

	// corrupted CRC: that frame is dropped, the next one is read
	{
		auto bad = good;
		bad[100] ^= 0x01;
		auto r = Feed(cat({ good, bad, good }), 64);
		CHECK(r.packets.size() == 2 && r.bad == 1 && ! r.tooMany, "bad CRC: got " << r.packets.size() << " packets, " << r.bad << " bad");
	}
	// garbage between frames, with a false "TCD" start in it
	{
		std::vector<uint8_t> junk = { 0x00, 0x11, 'T', 'C', 'D', 0x99, 0x42, 0x13 };
		auto r = Feed(cat({ good, junk, good, junk, junk, good }), 5);
		CHECK(r.packets.size() == 3 && r.bad == 2 && ! r.tooMany, "garbage: got " << r.packets.size() << " packets, " << r.bad << " bad");
	}
	// wrong version
	{
		auto bad = good;
		bad[4] = 2;
		auto r = Feed(cat({ bad, good }), 4096);
		CHECK(r.packets.size() == 1 && r.bad == 1, "version: got " << r.packets.size() << " packets, " << r.bad << " bad");
	}
	// length out of range
	{
		auto bad = good;
		bad[6] = 0x10;
		bad[7] = 0x00;
		auto r = Feed(cat({ bad, good }), 4096);
		CHECK(r.packets.size() == 1 && r.bad == 1, "length: got " << r.packets.size() << " packets, " << r.bad << " bad");
	}
	// a longer payload (a later version may add fields) is accepted
	{
		std::vector<uint8_t> f(good.begin(), good.begin() + 8 + TCPacketWireSize);
		f.insert(f.end(), { 1, 2, 3, 4 });
		f[6] = uint8_t((TCPacketWireSize + 4) & 0xFF);
		f[7] = uint8_t((TCPacketWireSize + 4) >> 8);
		uint32_t c = Crc32Ieee(f.data() + 4, 4 + TCPacketWireSize + 4);
		for (int i = 0; i < 4; i++) f.push_back(uint8_t(c >> (8 * i)));
		auto r = Feed(f, 4096);
		CHECK(r.packets.size() == 1 && r.bad == 0, "longer payload accepted");
	}
	// N bad frames in a row -> too_many_bad
	{
		auto bad = good;
		bad[200] ^= 0xFF;
		std::vector<uint8_t> s;
		for (unsigned i = 0; i < kV2MaxBadRun; i++) s.insert(s.end(), bad.begin(), bad.end());
		s.insert(s.end(), good.begin(), good.end());
		auto r = Feed(s, 4096);
		CHECK(r.tooMany && r.packets.empty(), "N bad in a row gives too_many_bad");
	}
	// N-1 bad frames, then a good one: no reconnect
	{
		auto bad = good;
		bad[200] ^= 0xFF;
		std::vector<uint8_t> s;
		for (unsigned i = 0; i + 1 < kV2MaxBadRun; i++) s.insert(s.end(), bad.begin(), bad.end());
		s.insert(s.end(), good.begin(), good.end());
		auto r = Feed(s, 4096);
		CHECK(! r.tooMany && r.packets.size() == 1, "N-1 bad then good recovers");
	}
	// mask rules
	CHECK(TCOutNormalize(0) == kTCOutAll, "mask 0 means all");
	CHECK(TCOutNormalize(0x01) == kTCOutAll, "mask with no known bit means all");
	CHECK(TCOutNormalize(TCOut(ECodecType::c2_1600)) == TCOut(ECodecType::c2_3200), "c2_1600 bit selects m17");
	CHECK(TCOutNormalize(0xFF) == kTCOutAll, "unknown bits dropped");
	CHECK(kTCOutAll == 0x76, "all = dstar|dmr|m17|p25|usrp = 0x76");
	{
		STCPacket p{};
		p.module = 'A';
		p.codec_in = ECodecType::dmr;
		CTranscoderPacket tp(p, TCOut(ECodecType::dmr));
		CHECK(tp.Needed() == 0, "only the input requested: nothing to transcode");
		CHECK(tp.AllCodecsAreSet(), "only the input requested: complete at once");
		CTranscoderPacket tq(p, TCOut(ECodecType::usrp));
		CHECK(tq.Needed() == TCOut(ECodecType::usrp), "usrp requested from dmr: usrp needed");
		CHECK(! tq.AllCodecsAreSet(), "usrp requested: not complete before the encode");
		int16_t a[160] = { 1 };
		tq.SetUSRPData(a);
		CHECK(tq.AllCodecsAreSet(), "usrp requested: complete after usrp is set");
		p.codec_in = ECodecType::c2_1600;
		CTranscoderPacket tc(p, TCOut(ECodecType::c2_3200) | TCOut(ECodecType::dmr));
		CHECK(tc.Needed() == TCOut(ECodecType::dmr), "c2_1600 input fills the m17 field");
	}

	// CRC check value of the standard test string
	{
		const char *t = "123456789";
		CHECK(Crc32Ieee(reinterpret_cast<const uint8_t *>(t), 9) == 0xCBF43926u, "CRC-32 check value");
	}

	std::cout << "codec_test: " << checks << " checks, " << failures << " failures" << std::endl;
	return failures ? 1 : 0;
}
