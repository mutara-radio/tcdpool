// Shared test vectors for the tcd link. The same file
// (testdata/tcd-wire-vectors.txt) is read by the Go tests in
// internal/backend/nostar. One vector per line, fields are key=value,
// separated by spaces. Lines that start with '#' are comments.
//
//   name    text
//   seq     u32, decimal
//   module  one character
//   last    0 or 1
//   sid     u16, decimal
//   codec   codec_in, decimal
//   dstar dmr m17 p25   hex bytes
//   usrp    160 decimal int16 values, separated by ','
//   raw     hex of the 376-byte raw wire packet
//   v2      hex of the 388-byte v2 frame
#pragma once

#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <fstream>
#include <map>
#include <sstream>
#include <string>
#include <vector>

struct SVector
{
	std::map<std::string, std::string> f;
};

static inline std::vector<uint8_t> FromHex(const std::string &h)
{
	std::vector<uint8_t> out;
	for (std::size_t i = 0; i + 1 < h.size(); i += 2)
		out.push_back(uint8_t(std::strtoul(h.substr(i, 2).c_str(), nullptr, 16)));
	return out;
}

static inline std::string ToHex(const uint8_t *p, std::size_t n)
{
	static const char *d = "0123456789abcdef";
	std::string s;
	for (std::size_t i = 0; i < n; i++)
	{
		s += d[p[i] >> 4];
		s += d[p[i] & 15];
	}
	return s;
}

static inline std::vector<SVector> ReadVectors(const char *path)
{
	std::vector<SVector> out;
	std::ifstream in(path);
	if (! in)
	{
		std::fprintf(stderr, "cannot open %s\n", path);
		std::exit(2);
	}
	std::string line;
	while (std::getline(in, line))
	{
		if (line.empty() || line[0] == '#')
			continue;
		SVector v;
		std::istringstream ss(line);
		std::string tok;
		while (ss >> tok)
		{
			auto eq = tok.find('=');
			if (eq != std::string::npos)
				v.f[tok.substr(0, eq)] = tok.substr(eq + 1);
		}
		out.push_back(v);
	}
	return out;
}
