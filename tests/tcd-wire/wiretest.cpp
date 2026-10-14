// Link test for a tcd binary. The test is the reflector: it listens on
// loopback, starts tcd, and sends good and bad input on the link.
//
// Usage: wiretest <tcd binary> raw|v2 [scenario ...]
//
// Each scenario starts a new tcd process. The result of each scenario is
// one line "PASS name" or "FAIL name: reason". The exit status is the
// count of failures. Run it against the unpatched tcd to see which
// scenarios the patches fix.

#include <algorithm>
#include <arpa/inet.h>
#include <cerrno>
#include <cstdio>
#include <csignal>
#include <cstring>
#include <fcntl.h>
#include <functional>
#include <iostream>
#include <netinet/in.h>
#include <netinet/tcp.h>
#include <poll.h>
#include <string>
#include <sys/socket.h>
#include <sys/wait.h>
#include <thread>
#include <atomic>
#include <unistd.h>
#include <vector>
#include <chrono>
#include <fstream>

#include "TCPacketCodec.h"
#include "TCFraming.h"

using Clock = std::chrono::steady_clock;

static std::string g_Bin;
static bool g_V2 = false;

static void msleep(int ms) { std::this_thread::sleep_for(std::chrono::milliseconds(ms)); }

struct STcd
{
	pid_t pid = -1;
	int lfd = -1;
	int fd = -1;
	CV2Reader rx;
	std::vector<uint8_t> rawbuf;
	std::string log;

	~STcd() { Stop(); }

	bool Start(const std::string &name, const std::string &extraIni = "")
	{
		lfd = socket(AF_INET, SOCK_STREAM, 0);
		int yes = 1;
		setsockopt(lfd, SOL_SOCKET, SO_REUSEADDR, &yes, sizeof(yes));
		sockaddr_in a{};
		a.sin_family = AF_INET;
		a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
		uint16_t port = 0;
		for (uint16_t p = 41000 + (getpid() % 500); p < 48000; p++)
		{
			a.sin_port = htons(p);
			if (0 == bind(lfd, (sockaddr *)&a, sizeof(a)))
			{
				port = p;
				break;
			}
		}
		if (! port || listen(lfd, 4))
			return false;
		const std::string ini = "/tmp/wiretest-" + name + ".ini";
		log = "/tmp/wiretest-" + name + ".log";
		std::ofstream f(ini);
		f << "Port = " << port << "\nServerAddress = 127.0.0.1\nModules = A\n"
		  << "DStarGainIn = 0\nDStarGainOut = 0\nDmrYsfGainIn = 0\nDmrYsfGainOut = 0\nUsrpTxGain = 0\nUsrpRxGain = 0\n";
		if (g_V2)
			f << "Framing = v2\n";
		f << extraIni;
		f.close();
		pid = fork();
		if (0 == pid)
		{
			int lf = open(log.c_str(), O_WRONLY | O_CREAT | O_TRUNC, 0644);
			dup2(lf, 1);
			dup2(lf, 2);
			close(lfd);
			execl(g_Bin.c_str(), g_Bin.c_str(), ini.c_str(), (char *)nullptr);
			_exit(127);
		}
		return Accept(15000);
	}

	// Accept the (next) connection from tcd and check the module byte.
	bool Accept(int ms)
	{
		pollfd p{ lfd, POLLIN, 0 };
		if (poll(&p, 1, ms) <= 0)
			return false;
		if (fd >= 0)
			close(fd);
		fd = accept(lfd, nullptr, nullptr);
		if (fd < 0)
			return false;
		int yes = 1;
		setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &yes, sizeof(yes));
		char mod = 0;
		if (1 != recv(fd, &mod, 1, MSG_WAITALL) || mod != 'A')
			return false;
		rx.Reset();
		rawbuf.clear();
		return true;
	}

	bool Alive()
	{
		int st;
		return pid > 0 && 0 == waitpid(pid, &st, WNOHANG);
	}

	void Stop()
	{
		if (fd >= 0) { close(fd); fd = -1; }
		if (lfd >= 0) { close(lfd); lfd = -1; }
		if (pid > 0)
		{
			kill(pid, SIGKILL);
			int st;
			waitpid(pid, &st, 0);
			pid = -1;
		}
	}

	bool SendBytes(const uint8_t *p, std::size_t n)
	{
		while (n)
		{
			auto k = send(fd, p, n, MSG_NOSIGNAL);
			if (k <= 0)
				return false;
			p += k;
			n -= std::size_t(k);
		}
		return true;
	}

	std::vector<uint8_t> Wire(const STCPacket &pk, uint8_t outputs = 0)
	{
		std::vector<uint8_t> b(g_V2 ? kV2FrameSize : TCPacketWireSize);
		if (g_V2)
			EncodeV2Frame(pk, outputs, b.data());
		else
			EncodeTCPacket(pk, b.data());
		return b;
	}

	// Waits for the next packet. Returns 1 for a packet, 0 on timeout,
	// -1 when tcd closed the link.
	// outputs gets the outputs byte of a v2 frame.
	int Read(STCPacket &out, int ms, bool skipPings = true, uint8_t *outputs = nullptr)
	{
		uint8_t dummy;
		if (! outputs)
			outputs = &dummy;
		auto end = Clock::now() + std::chrono::milliseconds(ms);
		for (;;)
		{
			// take a packet that is already buffered
			if (g_V2)
			{
				const char *why;
				for (;;)
				{
					auto st = rx.Next(out, *outputs, why);
					if (st == EV2Status::packet)
					{
						if (skipPings && out.codec_in == ECodecType::ping)
							continue;
						return 1;
					}
					if (st == EV2Status::need_more)
						break;
				}
			}
			else
			{
				while (rawbuf.size() >= TCPacketWireSize)
				{
					// raw bytes from tcd; decode leniently (unpatched pings
					// carry random bytes)
					std::memcpy(&out, rawbuf.data(), TCPacketWireSize);
					rawbuf.erase(rawbuf.begin(), rawbuf.begin() + TCPacketWireSize);
					if (skipPings && out.codec_in == ECodecType::ping)
						continue;
					return 1;
				}
			}
			auto left = std::chrono::duration_cast<std::chrono::milliseconds>(end - Clock::now()).count();
			if (left <= 0)
				return 0;
			pollfd p{ fd, POLLIN, 0 };
			if (poll(&p, 1, int(left)) <= 0)
				return 0;
			uint8_t b[4096];
			auto n = recv(fd, b, sizeof(b), 0);
			if (n <= 0)
				return -1;
			if (g_V2)
				rx.Append(b, std::size_t(n));
			else
				rawbuf.insert(rawbuf.end(), b, b + n);
		}
	}

	// True when tcd closes the link within ms (data before EOF is ignored).
	bool ClosedWithin(int ms)
	{
		auto end = Clock::now() + std::chrono::milliseconds(ms);
		while (Clock::now() < end)
		{
			pollfd p{ fd, POLLIN, 0 };
			if (poll(&p, 1, 100) > 0)
			{
				uint8_t b[4096];
				auto n = recv(fd, b, sizeof(b), 0);
				if (n <= 0)
					return true;
			}
		}
		return false;
	}
};

static STCPacket Usrp(uint32_t seq, uint16_t sid)
{
	STCPacket p{};
	p.sequence = seq;
	p.module = 'A';
	p.streamid = sid;
	p.codec_in = ECodecType::usrp;
	for (int i = 0; i < 160; i++)
		p.usrp[i] = int16_t(8000.0 * __builtin_sin(2 * 3.14159265 * 1000.0 * (seq * 160 + i) / 8000.0));
	return p;
}

static bool IsReplyTo(const STCPacket &r, uint32_t seq, uint16_t sid)
{
	return r.codec_in == ECodecType::usrp && r.sequence == seq && r.streamid == sid && r.module == 'A';
}

// Sends one good packet and expects its reply: proves the link works.
static std::string RoundTrip(STcd &t, uint32_t seq, uint16_t sid)
{
	auto w = t.Wire(Usrp(seq, sid));
	if (! t.SendBytes(w.data(), w.size()))
		return "send failed";
	STCPacket r;
	int rv = t.Read(r, 5000);
	if (rv != 1)
		return rv == 0 ? "no reply within 5 s" : "link closed";
	if (! IsReplyTo(r, seq, sid))
		return "reply does not match (seq " + std::to_string(r.sequence) + ")";
	return "";
}

// Sends a bad raw packet, expects tcd to close the link, reconnect, and
// answer a good packet on the new link.
static std::string BadThenRecover(STcd &t, const std::vector<uint8_t> &bad)
{
	if (! t.SendBytes(bad.data(), bad.size()))
		return "send failed";
	if (! t.ClosedWithin(3000))
		return "tcd did not close the link after a bad packet";
	if (! t.Accept(5000))
		return "tcd did not reconnect";
	return RoundTrip(t, 77, 0x0777);
}

// Which codec fields of a reply hold data, as an outputs mask.
static uint8_t Filled(const STCPacket &r)
{
	auto nz = [](const void *p, std::size_t n) {
		auto b = static_cast<const uint8_t *>(p);
		for (std::size_t i = 0; i < n; i++)
			if (b[i]) return true;
		return false;
	};
	uint8_t m = 0;
	if (nz(r.dstar, sizeof(r.dstar))) m |= TCOut(ECodecType::dstar);
	if (nz(r.dmr, sizeof(r.dmr))) m |= TCOut(ECodecType::dmr);
	if (nz(r.m17, sizeof(r.m17))) m |= TCOut(ECodecType::c2_3200);
	if (nz(r.p25, sizeof(r.p25))) m |= TCOut(ECodecType::p25);
	if (nz(r.usrp, sizeof(r.usrp))) m |= TCOut(ECodecType::usrp);
	return m;
}

static std::string Hex(unsigned v)
{
	char b[8];
	snprintf(b, sizeof(b), "0x%02x", v);
	return b;
}

// Sends one packet with a requested-outputs mask and checks that the
// reply holds exactly the expected fields and says so in its header.
static std::string MaskCase(STcd &t, const STCPacket &in, uint8_t outputs, uint8_t expect)
{
	auto w = t.Wire(in, outputs);
	if (! t.SendBytes(w.data(), w.size()))
		return "send failed";
	STCPacket r;
	uint8_t got = 0;
	int rv = t.Read(r, 5000, true, &got);
	if (rv != 1)
		return rv == 0 ? "no reply within 5 s" : "link closed";
	if (r.sequence != in.sequence || r.streamid != in.streamid)
		return "reply does not match";
	if (g_V2 && got != expect)
		return "reply header outputs " + Hex(got) + ", want " + Hex(expect);
	if (Filled(r) != expect)
		return "reply fields filled " + Hex(Filled(r)) + ", want " + Hex(expect);
	return "";
}

static STCPacket DStar(uint32_t seq, uint16_t sid)
{
	// a D-Star AMBE frame of a silent voice (any 9 bytes decode)
	static const uint8_t silence[9] = { 0x9e, 0x8d, 0x32, 0x88, 0x26, 0x1a, 0x3f, 0x61, 0xe8 };
	STCPacket p{};
	p.sequence = seq;
	p.module = 'A';
	p.streamid = sid;
	p.codec_in = ECodecType::dstar;
	std::memcpy(p.dstar, silence, 9);
	return p;
}

// Time from send to reply for n frames sent every 20 ms (microseconds).
static std::string Timing(STcd &t, const char *label, ECodecType in, uint8_t outputs, uint16_t sid)
{
	const int n = 100;
	std::vector<long> us;
	for (int i = 0; i < n; i++)
	{
		STCPacket p = Usrp(uint32_t(i), sid);
		if (in == ECodecType::dmr)
		{
			// any 9 bytes decode; the content does not change the time
			std::memset(p.usrp, 0, sizeof(p.usrp));
			p.codec_in = ECodecType::dmr;
			for (int k = 0; k < 9; k++) p.dmr[k] = uint8_t(0x5A ^ (i * 7 + k));
		}
		auto w = t.Wire(p, outputs);
		auto t0 = Clock::now();
		t.SendBytes(w.data(), w.size());
		STCPacket r;
		if (t.Read(r, 2000) != 1)
			return std::string(label) + ": no reply for frame " + std::to_string(i);
		us.push_back(long(std::chrono::duration_cast<std::chrono::microseconds>(Clock::now() - t0).count()));
		auto rest = std::chrono::milliseconds(20) - (Clock::now() - t0);
		if (rest.count() > 0)
			std::this_thread::sleep_for(rest);
	}
	std::sort(us.begin(), us.end());
	long sum = 0;
	for (auto v : us) sum += v;
	std::cout << "TIMING " << label << " outputs=" << Hex(outputs) << " n=" << n << " min_us=" << us.front()
	          << " avg_us=" << sum / n << " p99_us=" << us[size_t(n * 99 / 100) - 1] << " max_us=" << us.back() << std::endl;
	return "";
}

// Frames sent every 20 ms by one thread while this thread reads the
// replies, as a real client does. The test socket delays its ACKs (the
// Linux default); without TCP_NODELAY on the tcd side, Nagle's
// algorithm then holds each reply until the next frame comes in.
static std::string Paced(STcd &t, uint8_t outputs, uint16_t sid)
{
	const int n = 100;
	std::vector<Clock::time_point> sent(n);
	std::vector<long> us;
	std::atomic<bool> sendFailed{false};
	std::thread sender([&] {
		auto next = Clock::now();
		for (int i = 0; i < n; i++)
		{
			STCPacket p = Usrp(uint32_t(i), sid);
			auto w = t.Wire(p, outputs);
			sent[i] = Clock::now();
			if (! t.SendBytes(w.data(), w.size()))
			{
				sendFailed = true;
				return;
			}
			next += std::chrono::milliseconds(20);
			std::this_thread::sleep_until(next);
		}
	});
	std::string err;
	for (int got = 0; got < n; got++)
	{
		STCPacket r;
		if (t.Read(r, 2000) != 1)
		{
			err = "no reply after " + std::to_string(got) + " replies";
			break;
		}
		auto now = Clock::now();
		if (r.sequence >= uint32_t(n))
		{
			err = "reply with a bad sequence";
			break;
		}
		us.push_back(long(std::chrono::duration_cast<std::chrono::microseconds>(now - sent[r.sequence]).count()));
	}
	sender.join();
	if (! err.empty())
		return err;
	if (sendFailed)
		return "send failed";
	std::sort(us.begin(), us.end());
	long sum = 0;
	for (auto v : us) sum += v;
	const long avg = sum / long(us.size());
	std::cout << "PACED outputs=" << Hex(outputs) << " n=" << us.size() << " min_us=" << us.front() << " avg_us=" << avg
	          << " p50_us=" << us[us.size() / 2] << " max_us=" << us.back() << std::endl;
	if (avg > 10000)
		return "average reply time " + std::to_string(avg) + " us is more than 10 ms (Nagle on the tcd socket?)";
	return "";
}

struct SScenario
{
	const char *name;
	bool v2;
	std::function<std::string(STcd &)> run;
	const char *ini = "";
};

int main(int argc, char **argv)
{
	if (argc < 3)
	{
		std::cerr << "usage: wiretest <tcd> raw|v2 [scenario...]" << std::endl;
		return 2;
	}
	g_Bin = argv[1];
	g_V2 = std::string(argv[2]) == "v2";
	signal(SIGPIPE, SIG_IGN);

	std::vector<SScenario> all = {
		{ "raw_split_writes", false, [](STcd &t) -> std::string {
			  auto w = t.Wire(Usrp(10, 0x0101));
			  t.SendBytes(w.data(), 100); msleep(20);
			  t.SendBytes(w.data() + 100, 150); msleep(20);
			  t.SendBytes(w.data() + 250, w.size() - 250);
			  STCPacket r;
			  if (t.Read(r, 5000) != 1) return "no reply";
			  if (! IsReplyTo(r, 10, 0x0101)) return "wrong reply";
			  bool dmr = false;
			  for (auto b : r.dmr) dmr |= b != 0;
			  return dmr ? "" : "reply has no AMBE+2 data";
		  } },
		{ "raw_byte_by_byte", false, [](STcd &t) -> std::string {
			  auto w = t.Wire(Usrp(11, 0x0102));
			  for (std::size_t i = 0; i < w.size(); i++) { t.SendBytes(w.data() + i, 1); if (i % 50 == 0) msleep(2); }
			  STCPacket r;
			  if (t.Read(r, 5000) != 1) return "no reply";
			  return IsReplyTo(r, 11, 0x0102) ? "" : "wrong reply";
		  } },
		{ "raw_truncated_then_close", false, [](STcd &t) -> std::string {
			  // half a packet, then close: tcd must drop it, not process it
			  auto w = t.Wire(Usrp(20, 0x0202));
			  t.SendBytes(w.data(), 200);
			  msleep(50);
			  close(t.fd); t.fd = -1;
			  if (! t.Accept(5000)) return "tcd did not reconnect";
			  STCPacket r;
			  int rv = t.Read(r, 3000);
			  if (rv == 1) return "tcd processed the partial packet and sent a reply for seq " + std::to_string(r.sequence);
			  return RoundTrip(t, 21, 0x0202);
		  } },
		{ "raw_wrong_module", false, [](STcd &t) -> std::string {
			  auto p = Usrp(30, 0x0303);
			  p.module = 'Z';
			  return BadThenRecover(t, t.Wire(p));
		  } },
		{ "raw_bad_codec", false, [](STcd &t) -> std::string {
			  auto w = t.Wire(Usrp(31, 0x0303));
			  w[8] = 7;
			  return BadThenRecover(t, w);
		  } },
		{ "raw_bad_bool", false, [](STcd &t) -> std::string {
			  auto w = t.Wire(Usrp(32, 0x0303));
			  w[5] = 2;
			  return BadThenRecover(t, w);
		  } },
		{ "raw_ping_is_clean", false, [](STcd &t) -> std::string {
			  STCPacket r;
			  // a ping comes about every 5 s
			  int rv = t.Read(r, 7000, false);
			  if (rv != 1) return "no ping within 7 s";
			  if (r.codec_in != ECodecType::ping) return "first packet is not a ping";
			  uint8_t raw[TCPacketWireSize];
			  std::memcpy(raw, &r, sizeof(raw));
			  for (std::size_t i = 0; i < TCPacketWireSize; i++)
				  if (i != 4 && i != 8 && raw[i] != 0)
					  return "ping byte " + std::to_string(i) + " is not zero (uninitialized memory on the wire)";
			  return r.module == 'A' ? "" : "ping module byte is not 'A'";
		  } },
		{ "raw_default_outputs", false, [](STcd &t) -> std::string {
			  // DefaultOutputs in tcd.ini sets the outputs of a raw link
			  // (the unpatched tcd ignores the key and fills every codec)
			  const uint8_t m = TCOut(ECodecType::dmr) | TCOut(ECodecType::usrp);
			  return MaskCase(t, Usrp(60, 0x0606), 0, m);
		  }, "DefaultOutputs = dmr, usrp\n" },
		{ "raw_peer_closes_with_replies_pending", false, [](STcd &t) -> std::string {
			  // many packets, then close at once: tcd sends replies into a
			  // closed socket and must survive (no SIGPIPE)
			  for (uint32_t s = 0; s < 50; s++) { auto w = t.Wire(Usrp(100 + s, 0x0404)); t.SendBytes(w.data(), w.size()); }
			  close(t.fd); t.fd = -1;
			  msleep(1500);
			  if (! t.Alive()) return "tcd died (SIGPIPE?)";
			  if (! t.Accept(5000)) return "tcd did not reconnect";
			  msleep(500);
			  if (! t.Alive()) return "tcd died after reconnect";
			  // drain replies that were pending for the old link
			  STCPacket r;
			  while (t.Read(r, 500) == 1) {}
			  return RoundTrip(t, 200, 0x0405);
		  } },

		{ "raw_paced_latency", false, [](STcd &t) -> std::string { return Paced(t, 0, 0x1301); } },
		{ "v2_paced_latency", true, [](STcd &t) -> std::string { return Paced(t, TCOut(ECodecType::dmr), 0x1302); } },
		{ "v2_round_trip", true, [](STcd &t) -> std::string { return RoundTrip(t, 1, 0x1001); } },
		{ "v2_split_writes", true, [](STcd &t) -> std::string {
			  auto w = t.Wire(Usrp(2, 0x1002));
			  std::size_t sizes[] = { 1, 2, 3, 5, 8, 13, 21, 34, 55, 89 };
			  std::size_t off = 0, k = 0;
			  while (off < w.size()) { auto n = std::min(sizes[k++ % 10], w.size() - off); t.SendBytes(w.data() + off, n); off += n; msleep(3); }
			  STCPacket r;
			  if (t.Read(r, 5000) != 1) return "no reply";
			  return IsReplyTo(r, 2, 0x1002) ? "" : "wrong reply";
		  } },
		{ "v2_bad_crc_resync", true, [](STcd &t) -> std::string {
			  auto bad = t.Wire(Usrp(3, 0x1003));
			  bad[150] ^= 0x40;
			  auto good = t.Wire(Usrp(4, 0x1003));
			  t.SendBytes(bad.data(), bad.size());
			  t.SendBytes(good.data(), good.size());
			  STCPacket r;
			  if (t.Read(r, 5000) != 1) return "no reply after a bad CRC frame";
			  if (! IsReplyTo(r, 4, 0x1003)) return "reply is for seq " + std::to_string(r.sequence) + ", not 4";
			  if (t.Read(r, 500) == -1) return "link closed";
			  return "";
		  } },
		{ "v2_garbage_between_frames", true, [](STcd &t) -> std::string {
			  uint8_t junk[] = { 0x00, 'T', 'C', 'D', 0x01, 0xFF, 0x13, 0x37, 'T', 'C' };
			  auto a = t.Wire(Usrp(5, 0x1004));
			  auto b = t.Wire(Usrp(6, 0x1004));
			  t.SendBytes(junk, sizeof(junk));
			  t.SendBytes(a.data(), a.size());
			  t.SendBytes(junk, sizeof(junk));
			  t.SendBytes(b.data(), b.size());
			  STCPacket r1, r2;
			  if (t.Read(r1, 5000) != 1 || t.Read(r2, 5000) != 1) return "missing replies";
			  return (IsReplyTo(r1, 5, 0x1004) && IsReplyTo(r2, 6, 0x1004)) ? "" : "wrong replies";
		  } },
		{ "v2_wrong_version", true, [](STcd &t) -> std::string {
			  auto bad = t.Wire(Usrp(7, 0x1005));
			  bad[4] = 9;
			  auto good = t.Wire(Usrp(8, 0x1005));
			  t.SendBytes(bad.data(), bad.size());
			  t.SendBytes(good.data(), good.size());
			  STCPacket r;
			  if (t.Read(r, 5000) != 1) return "no reply";
			  return IsReplyTo(r, 8, 0x1005) ? "" : "wrong reply";
		  } },
		{ "v2_wrong_module_dropped", true, [](STcd &t) -> std::string {
			  auto p = Usrp(9, 0x1006);
			  p.module = 'Z';
			  auto bad = t.Wire(p);
			  auto good = t.Wire(Usrp(10, 0x1006));
			  t.SendBytes(bad.data(), bad.size());
			  t.SendBytes(good.data(), good.size());
			  STCPacket r;
			  if (t.Read(r, 5000) != 1) return "no reply";
			  return IsReplyTo(r, 10, 0x1006) ? "" : "wrong reply";
		  } },
		{ "v2_too_many_bad_reconnects", true, [](STcd &t) -> std::string {
			  auto bad = t.Wire(Usrp(11, 0x1007));
			  bad[300] ^= 0x01;
			  for (unsigned i = 0; i < kV2MaxBadRun; i++) t.SendBytes(bad.data(), bad.size());
			  if (! t.ClosedWithin(3000)) return "tcd did not close after " + std::to_string(kV2MaxBadRun) + " bad frames";
			  if (! t.Accept(5000)) return "tcd did not reconnect";
			  return RoundTrip(t, 12, 0x1007);
		  } },
		{ "v2_mask_all_when_zero", true, [](STcd &t) -> std::string {
			  return MaskCase(t, Usrp(40, 0x1100), 0, kTCOutAll);
		  } },
		{ "v2_mask_usrp_to_dmr_only", true, [](STcd &t) -> std::string {
			  // the input field is not echoed when it is not requested
			  return MaskCase(t, Usrp(41, 0x1101), TCOut(ECodecType::dmr), TCOut(ECodecType::dmr));
		  } },
		{ "v2_mask_echo_only", true, [](STcd &t) -> std::string {
			  return MaskCase(t, Usrp(42, 0x1102), TCOut(ECodecType::usrp), TCOut(ECodecType::usrp));
		  } },
		{ "v2_mask_dstar_to_usrp", true, [](STcd &t) -> std::string {
			  return MaskCase(t, DStar(43, 0x1103), TCOut(ECodecType::usrp), TCOut(ECodecType::usrp));
		  } },
		{ "v2_mask_two_outputs", true, [](STcd &t) -> std::string {
			  const uint8_t m = TCOut(ECodecType::dstar) | TCOut(ECodecType::p25);
			  return MaskCase(t, Usrp(44, 0x1104), m, m);
		  } },
		{ "v2_mask_c2_1600_bit_means_m17", true, [](STcd &t) -> std::string {
			  return MaskCase(t, Usrp(46, 0x1105), TCOut(ECodecType::c2_1600), TCOut(ECodecType::c2_3200));
		  } },
		{ "v2_mask_per_packet", true, [](STcd &t) -> std::string {
			  // the mask can change from one packet to the next
			  for (uint32_t i = 0; i < 6; i++)
			  {
				  const uint8_t m = (i % 2) ? TCOut(ECodecType::dmr) : TCOut(ECodecType::p25);
				  auto e = MaskCase(t, Usrp(50 + i, 0x1106), m, m);
				  if (! e.empty()) return "packet " + std::to_string(i) + ": " + e;
			  }
			  return "";
		  } },
		{ "v2_mask_timing", true, [](STcd &t) -> std::string {
			  // prints latency lines; fails only when replies are missing
			  std::string e;
			  e += Timing(t, "usrp->dmr all", ECodecType::usrp, 0, 0x1201);
			  e += Timing(t, "usrp->dmr mask", ECodecType::usrp, TCOut(ECodecType::dmr), 0x1202);
			  e += Timing(t, "dmr->usrp all", ECodecType::dmr, 0, 0x1203);
			  e += Timing(t, "dmr->usrp mask", ECodecType::dmr, TCOut(ECodecType::usrp), 0x1204);
			  return e;
		  } },
		{ "v2_ping_is_framed", true, [](STcd &t) -> std::string {
			  STCPacket r;
			  int rv = t.Read(r, 7000, false);
			  if (rv != 1) return "no v2 ping within 7 s";
			  return r.codec_in == ECodecType::ping ? "" : "first frame is not a ping";
		  } },
	};

	std::vector<std::string> only(argv + 3, argv + argc);
	int failures = 0, ran = 0;
	for (auto &s : all)
	{
		if (s.v2 != g_V2)
			continue;
		if (! only.empty() && std::find(only.begin(), only.end(), s.name) == only.end())
			continue;
		ran++;
		STcd t;
		std::string err;
		if (! t.Start(s.name, s.ini))
			err = "tcd did not connect";
		else
			err = s.run(t);
		t.Stop();
		if (err.empty())
			std::cout << "PASS " << s.name << std::endl;
		else
		{
			failures++;
			std::cout << "FAIL " << s.name << ": " << err << " (log " << t.log << ")" << std::endl;
		}
	}
	std::cout << "wiretest " << g_Bin << " " << argv[2] << ": " << ran << " scenarios, " << failures << " failed" << std::endl;
	return failures;
}
