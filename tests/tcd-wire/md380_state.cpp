// Checks whether the AMBE+2 encoder and decoder of the md380 emulator
// share state. tcd runs both on one thread (swambe2) through one global
// emulator. If they are independent, one tcd process can carry an
// encode stream and a decode stream at the same time with no change to
// either output.
//
// Each case runs in a new child process with a new emulator, and prints
// a hash of the AMBE+2 frames or the PCM it made:
//
//   enc_alone   encode stream A only
//   enc_mixed   encode stream A, with a decode of stream D after each frame
//   dec_alone   decode stream D only
//   dec_mixed   decode stream D, with an encode of stream B after each frame
//   dec_two     decode D, with a decode of stream E after each frame
//               (two decode streams in one process)
//
// enc_alone == enc_mixed and dec_alone == dec_mixed: encode and decode
// do not share state. dec_two != dec_alone is expected: decoders keep
// state from frame to frame, so two decode streams must not share one
// emulator.

#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

#include <md380_vocoder.h>

static const int kFrames = 150;

static uint64_t fnv(uint64_t h, const void *p, std::size_t n)
{
	auto b = static_cast<const uint8_t *>(p);
	for (std::size_t i = 0; i < n; i++)
	{
		h ^= b[i];
		h *= 1099511628211ull;
	}
	return h;
}

// A speech-like test signal: a 120 Hz pulse train through two formant
// resonators, with a syllable envelope. seed changes pitch and formants.
static void Signal(int seed, int frame, int16_t *out)
{
	static double y1[4], y2[4], z1[4], z2[4];
	const double f0 = 110.0 + 25.0 * seed;
	const double fa = 700.0 + 150.0 * seed, fb = 1200.0 + 300.0 * seed;
	auto coef = [](double f, double bw, double &a1, double &a2) {
		const double r = std::exp(-M_PI * bw / 8000.0);
		a1 = 2 * r * std::cos(2 * M_PI * f / 8000.0);
		a2 = -r * r;
	};
	double a1, a2, b1, b2;
	coef(fa, 90, a1, a2);
	coef(fb, 110, b1, b2);
	for (int i = 0; i < 160; i++)
	{
		const long n = long(frame) * 160 + i;
		const double t = double(n) / 8000.0;
		const double x = (std::fmod(t * f0, 1.0) < f0 / 8000.0) ? 1.0 : 0.0;
		const double y = x + a1 * y1[seed] + a2 * y2[seed];
		y2[seed] = y1[seed];
		y1[seed] = y;
		const double z = y + b1 * z1[seed] + b2 * z2[seed];
		z2[seed] = z1[seed];
		z1[seed] = z;
		const double env = 0.5 + 0.5 * std::sin(2 * M_PI * 3.0 * t);
		double v = 600.0 * z * env;
		if (v > 32000) v = 32000;
		if (v < -32000) v = -32000;
		out[i] = int16_t(v);
	}
}

// Encoded AMBE+2 streams D and E, made once in a separate child, so the
// decode cases all read the same input.
static std::vector<std::vector<uint8_t>> g_D, g_E;

static std::string RunCase(const std::string &name)
{
	int fds[2];
	if (pipe(fds))
		return "pipe";
	pid_t pid = fork();
	if (0 == pid)
	{
		close(fds[0]);
		md380_init();
		uint64_t h = 1469598103934665603ull;
		int16_t pcm[160], tmp[160];
		uint8_t ambe[9];
		for (int f = 0; f < kFrames; f++)
		{
			if (name == "enc_alone" || name == "enc_mixed")
			{
				Signal(0, f, pcm);
				md380_encode_fec(ambe, pcm);
				h = fnv(h, ambe, 9);
				if (name == "enc_mixed")
					md380_decode_fec(g_D[f].data(), tmp);
			}
			else
			{
				md380_decode_fec(g_D[f].data(), pcm);
				h = fnv(h, pcm, sizeof(pcm));
				if (name == "dec_mixed")
				{
					Signal(1, f, tmp);
					md380_encode_fec(ambe, tmp);
				}
				else if (name == "dec_two")
					md380_decode_fec(g_E[f].data(), tmp);
			}
		}
		char line[64];
		int n = snprintf(line, sizeof(line), "%016llx", (unsigned long long)h);
		if (write(fds[1], line, size_t(n)) != n)
			_exit(1);
		_exit(0);
	}
	close(fds[1]);
	char buf[64] = { 0 };
	ssize_t n = read(fds[0], buf, sizeof(buf) - 1);
	close(fds[0]);
	int st;
	waitpid(pid, &st, 0);
	return n > 0 ? std::string(buf, size_t(n)) : "error";
}

static void MakeStreams()
{
	int fds[2];
	if (pipe(fds))
		return;
	pid_t pid = fork();
	if (0 == pid)
	{
		close(fds[0]);
		md380_init();
		int16_t pcm[160];
		uint8_t ambe[9];
		for (int seed : { 2, 3 })
			for (int f = 0; f < kFrames; f++)
			{
				Signal(seed, f, pcm);
				md380_encode_fec(ambe, pcm);
				if (write(fds[1], ambe, 9) != 9)
					_exit(1);
			}
		_exit(0);
	}
	close(fds[1]);
	for (int s = 0; s < 2; s++)
		for (int f = 0; f < kFrames; f++)
		{
			std::vector<uint8_t> a(9);
			std::size_t got = 0;
			while (got < 9)
			{
				ssize_t n = read(fds[0], a.data() + got, 9 - got);
				if (n <= 0)
					break;
				got += std::size_t(n);
			}
			(s ? g_E : g_D).push_back(a);
		}
	close(fds[0]);
	int st;
	waitpid(pid, &st, 0);
}

int main()
{
	MakeStreams();
	if (g_D.size() != std::size_t(kFrames) || g_E.size() != std::size_t(kFrames))
	{
		printf("FAIL md380_state: could not make the test streams\n");
		return 1;
	}
	const auto ea = RunCase("enc_alone"), em = RunCase("enc_mixed");
	const auto da = RunCase("dec_alone"), dm = RunCase("dec_mixed"), d2 = RunCase("dec_two");
	const auto ea2 = RunCase("enc_alone");
	printf("md380_state: enc_alone=%s enc_alone_again=%s enc_mixed=%s\n", ea.c_str(), ea2.c_str(), em.c_str());
	printf("md380_state: dec_alone=%s dec_mixed=%s dec_two=%s\n", da.c_str(), dm.c_str(), d2.c_str());
	int fail = 0;
	if (ea != ea2)
	{
		printf("FAIL md380_state: the encoder is not deterministic\n");
		fail = 1;
	}
	printf("%s md380_state: encode with decodes in between %s\n", ea == em ? "PASS" : "FAIL", ea == em ? "gives the same AMBE+2" : "gives different AMBE+2 (shared state)");
	printf("%s md380_state: decode with encodes in between %s\n", da == dm ? "PASS" : "FAIL", da == dm ? "gives the same PCM" : "gives different PCM (shared state)");
	printf("INFO md380_state: two decode streams in one emulator %s\n", da == d2 ? "do not interfere" : "interfere (decoder state is shared)");
	if (ea != em || da != dm)
		fail = 1;
	return fail;
}
