// SPDX-License-Identifier: MIT
#pragma once
#include <algorithm>
#include <cstdint>
#include <limits>
#include <stdexcept>
#include <string>
#include <utility>
#include <vector>

namespace wire {
using Bytes = std::vector<uint8_t>;
constexpr uint32_t MaxPayload = 64u * 1024u * 1024u;
struct Error : std::runtime_error {
    std::string code;
    int exitCode;
    Error(std::string c, std::string message, int e = 1)
        : std::runtime_error(std::move(message)), code(std::move(c)), exitCode(e) {}
};
inline void be32(Bytes& b, uint32_t n) {
    for (int i = 24; i >= 0; i -= 8) b.push_back(uint8_t(n >> i));
}
inline void be64(Bytes& b, uint64_t n) {
    for (int i = 56; i >= 0; i -= 8) b.push_back(uint8_t(n >> i));
}
inline uint64_t readBE(const uint8_t* p, size_t n) {
    uint64_t v = 0;
    for (size_t i = 0; i < n; ++i) v = (v << 8) | p[i];
    return v;
}
inline size_t rgbaSize(uint32_t w, uint32_t h) {
    const uint64_t n = uint64_t(w) * h * 4;
    if (!w || !h || w > 8192 || h > 8192 || n + 16 > MaxPayload)
        throw Error("frame_too_large", "RGBA dimensions exceed protocol limits", 6);
    return size_t(n);
}
inline std::string quote(const std::string& s) {
    std::string r = "\"";
    const char* hex = "0123456789abcdef";
    for (unsigned char c : s) {
        if (c == '"' || c == '\\') { r += '\\'; r += char(c); }
        else if (c < 32) { r += "\\u00"; r += hex[c >> 4]; r += hex[c & 15]; }
        else r += char(c);
    }
    return r + '"';
}
struct Record { uint8_t type = 0; Bytes payload; };
inline Record json(uint8_t type, const std::string& text) {
    return {type, Bytes(text.begin(), text.end())};
}

// Strict, bounded, flat JSON control parser. Never logs untrusted input.
struct Control { std::string action; uint32_t bitrate = 0; };
inline bool control(const std::string& s, Control& result) {
    if (s.size() > 4096) return false;
    size_t i = 0;
    auto space = [&] { while (i < s.size() && (s[i]==' ' || s[i]=='\t' || s[i]=='\r')) ++i; };
    auto token = [&](char c) { space(); if (i == s.size() || s[i] != c) return false; ++i; return true; };
    auto string = [&](std::string& value) {
        if (!token('"')) return false;
        while (i < s.size()) {
            unsigned char c = s[i++];
            if (c == '"') return true;
            if (c < 32) return false;
            if (c == '\\') {
                if (i == s.size()) return false;
                char e = s[i++];
                if (e != '"' && e != '\\' && e != '/') return false;
                c = static_cast<unsigned char>(e);
            }
            value += char(c);
        }
        return false;
    };
    Control c;
    bool action = false, bitrate = false;
    if (!token('{')) return false;
    for (int count = 0; count < 8; ++count) {
        std::string key;
        if (!string(key) || !token(':')) return false;
        if (key == "action" && !action) {
            if (!string(c.action)) return false;
            action = true;
        } else if (key == "bitrate" && !bitrate) {
            space();
            const size_t start = i;
            uint64_t n = 0;
            while (i < s.size() && s[i] >= '0' && s[i] <= '9') {
                n = n * 10 + unsigned(s[i++] - '0');
                if (n > 200000000) return false;
            }
            if (i == start || (i - start > 1 && s[start] == '0') || n < 100000) return false;
            c.bitrate = uint32_t(n); bitrate = true;
        } else return false;
        space();
        if (i < s.size() && s[i] == '}') {
            ++i; space();
            if (i != s.size() || !action) return false;
            if (c.action == "bitrate" ? !bitrate : (bitrate || (c.action != "keyframe" && c.action != "stop")))
                return false;
            result = std::move(c);
            return true;
        }
        if (!token(',')) return false;
    }
    return false;
}

using Nals = std::vector<Bytes>;
inline Nals annex(const Bytes& b) {
    Nals result;
    size_t begin = 0;
    bool started = false;
    for (size_t i = 0; i + 2 < b.size();) {
        size_t prefix = 0;
        if (b[i] == 0 && b[i+1] == 0) {
            if (b[i+2] == 1) prefix = 3;
            else if (i+3 < b.size() && b[i+2] == 0 && b[i+3] == 1) prefix = 4;
        }
        if (!prefix) { ++i; continue; }
        if (!started) {
            for (size_t j = 0; j < i; ++j) if (b[j] != 0)
                throw Error("decoder_format", "Annex-B has leading nonzero bytes");
        } else {
            size_t end = i;
            while (end > begin && b[end-1] == 0) --end;
            if (end == begin) throw Error("decoder_format", "Empty Annex-B NAL");
            result.emplace_back(b.begin() + begin, b.begin() + end);
        }
        started = true;
        begin = i + prefix; i = begin;
        if (result.size() > 4096) throw Error("decoder_format", "Too many NAL units");
    }
    if (!started) throw Error("decoder_format", "Annex-B start code missing");
    size_t end = b.size();
    while (end > begin && b[end-1] == 0) --end;
    if (end == begin) throw Error("decoder_format", "Empty trailing NAL");
    result.emplace_back(b.begin()+begin, b.begin()+end);
    for (const auto& n : result) if (n.empty() || (n[0] & 0x80) || (n[0] & 31) == 0)
        throw Error("decoder_format", "Invalid NAL header");
    return result;
}
inline Bytes join(const Nals& nals) {
    Bytes b;
    for (const auto& n : nals) {
        if (n.empty() || b.size() + n.size() + 4 > MaxPayload - 9)
            throw Error("payload_too_large", "Encoded access unit exceeds limit", 6);
        b.insert(b.end(), {0,0,0,1}); b.insert(b.end(), n.begin(), n.end());
    }
    return b;
}
inline Nals avc(const Bytes& b, unsigned lengthSize) {
    if (lengthSize < 1 || lengthSize > 4) throw Error("encoder_format", "Invalid AVC length size");
    Nals result;
    for (size_t pos = 0; pos < b.size();) {
        if (b.size() - pos < lengthSize) throw Error("encoder_format", "Truncated AVC length");
        size_t n = size_t(readBE(b.data()+pos, lengthSize)); pos += lengthSize;
        if (!n || n > b.size()-pos || result.size() >= 4096)
            throw Error("encoder_format", "Invalid AVC NAL length");
        result.emplace_back(b.begin()+pos, b.begin()+pos+n); pos += n;
    }
    if (result.empty()) throw Error("encoder_format", "Empty AVC access unit");
    return result;
}
inline bool startsAnnex(const Bytes& b) {
    size_t zeros = 0;
    while (zeros < b.size() && b[zeros] == 0) ++zeros;
    return zeros >= 2 && zeros < b.size() && b[zeros] == 1;
}
inline Nals encodedNals(const Bytes& b, unsigned lengthSize) {
    return startsAnnex(b) ? annex(b) : avc(b, lengthSize);
}
inline Nals sequenceHeader(const Bytes& b, unsigned& lengthSize) {
    if (b.empty()) return {};
    if (startsAnnex(b)) return annex(b);
    if (b.size() < 7 || b[0] != 1) throw Error("encoder_format", "Unsupported sequence header");
    lengthSize = (b[4] & 3) + 1;
    size_t pos = 6;
    Nals nals;
    auto sets = [&](unsigned count) {
        for (unsigned k = 0; k < count; ++k) {
            if (pos + 2 > b.size()) throw Error("encoder_format", "Truncated avcC");
            size_t n = size_t(readBE(b.data()+pos, 2)); pos += 2;
            if (!n || n > b.size()-pos) throw Error("encoder_format", "Invalid avcC parameter set");
            nals.emplace_back(b.begin()+pos, b.begin()+pos+n); pos += n;
        }
    };
    sets(b[5] & 31);
    if (pos == b.size()) throw Error("encoder_format", "avcC PPS count missing");
    sets(b[pos++]);
    return nals;
}
class Bits {
    Bytes bytes_;
    size_t bit_ = 0;
public:
    explicit Bits(const Bytes& nal) {
        unsigned zeros = 0;
        for (size_t i = 1; i < nal.size(); ++i) {
            const uint8_t c = nal[i];
            if (zeros >= 2 && c == 3) {
                if (i+1 >= nal.size() || nal[i+1] > 3) throw Error("decoder_format", "Invalid RBSP escape");
                zeros = 0; continue;
            }
            bytes_.push_back(c);
            zeros = c == 0 ? zeros+1 : 0;
        }
    }
    uint32_t get(unsigned n) {
        if (n > 32 || n > bytes_.size()*8 - std::min(bit_, bytes_.size()*8))
            throw Error("decoder_format", "Truncated H264 syntax");
        uint32_t v = 0;
        for (unsigned i=0; i<n; ++i, ++bit_) v = (v << 1) | ((bytes_[bit_/8] >> (7-bit_%8)) & 1);
        return v;
    }
    uint32_t ue() {
        unsigned n = 0;
        while (!get(1)) if (++n > 30) throw Error("decoder_format", "H264 integer too large");
        return ((uint32_t(1) << n)-1) + get(n);
    }
    int32_t se() {
        const uint32_t v = ue();
        return v & 1 ? int32_t((v+1)/2) : -int32_t(v/2);
    }
};
struct Dimensions {
    uint32_t codedWidth=0, codedHeight=0, width=0, height=0, left=0, top=0;
    uint8_t profile=0, constraints=0, level=0;
};
inline Dimensions sps(const Bytes& nal) {
    if (nal.size() < 5 || (nal[0] & 31) != 7) throw Error("decoder_format", "Invalid SPS");
    Bits r(nal); Dimensions d;
    d.profile=uint8_t(r.get(8)); d.constraints=uint8_t(r.get(8)); d.level=uint8_t(r.get(8)); r.ue();
    if (d.profile == 100 || d.profile == 110 || d.profile == 122 || d.profile == 244 ||
        d.profile == 44 || d.profile == 83 || d.profile == 86 || d.profile == 118 ||
        d.profile == 128 || d.profile == 138 || d.profile == 139 || d.profile == 134 || d.profile == 135) {
        if (r.ue() != 1) throw Error("decoder_format", "Only H264 4:2:0 is supported");
        if (r.ue() != 0 || r.ue() != 0) throw Error("decoder_format", "Only 8 bit H264 is supported");
        r.get(1);
        if (r.get(1)) for (unsigned i=0; i<8; ++i) if (r.get(1)) {
            int last=8, next=8;
            for (unsigned j=0; j<(i<6 ? 16u:64u); ++j) {
                if (next) next=(last+r.se()+256)%256;
                last=next ? next:last;
            }
        }
    } else if (d.profile != 66 && d.profile != 77 && d.profile != 88)
        throw Error("decoder_format", "Unsupported H264 profile");
    if (r.ue() > 12) throw Error("decoder_format", "Invalid SPS frame number size");
    const uint32_t poc = r.ue();
    if (poc == 0) { if (r.ue() > 12) throw Error("decoder_format", "Invalid POC size"); }
    else if (poc == 1) {
        r.get(1); r.se(); r.se(); const uint32_t n = r.ue();
        if (n > 255) throw Error("decoder_format", "Invalid POC cycle");
        for (uint32_t i=0; i<n; ++i) r.se();
    } else if (poc != 2) throw Error("decoder_format", "Invalid POC type");
    r.ue(); r.get(1);
    const uint64_t wm = uint64_t(r.ue())+1, hm=uint64_t(r.ue())+1;
    const uint32_t progressive = r.get(1);
    if (!progressive) throw Error("decoder_format", "Interlaced H264 is unsupported");
    r.get(1);
    uint64_t left=0, right=0, top=0, bottom=0;
    if (r.get(1)) { left=r.ue(); right=r.ue(); top=r.ue(); bottom=r.ue(); }
    if (wm > 512 || hm > 512 || (left+right)*2 >= wm*16 || (top+bottom)*2 >= hm*16)
        throw Error("decoder_format", "Invalid SPS crop or dimensions");
    d.codedWidth=uint32_t(wm*16); d.codedHeight=uint32_t(hm*16);
    d.left=uint32_t(left*2); d.top=uint32_t(top*2);
    d.width=uint32_t(wm*16-(left+right)*2); d.height=uint32_t(hm*16-(top+bottom)*2);
    rgbaSize(d.width, d.height);
    return d;
}
inline bool sliceIsB(const Bytes& n) {
    if (n.empty()) throw Error("decoder_format", "Empty slice");
    const unsigned t = n[0] & 31;
    if (t != 1 && t != 5) return false;
    Bits r(n); r.ue(); uint32_t type=r.ue();
    if (type > 9) throw Error("decoder_format", "Invalid slice type");
    return type % 5 == 1;
}
struct AccessUnit {
    Bytes annexB;
    bool idr = false;
    bool slices = false;
    bool parameterChange = false;
};
struct ParameterSets {
    Bytes spsBytes, ppsBytes;
    void update(const Nals& nals) {
        for (const auto& n : nals) {
            if (n.empty() || (n[0] & 0x80)) throw Error("decoder_format", "Invalid NAL header");
            if ((n[0]&31) == 7) { sps(n); spsBytes=n; }
            else if ((n[0]&31) == 8) { if (n.size()<2) throw Error("decoder_format", "Invalid PPS"); ppsBytes=n; }
        }
    }
    AccessUnit au(const Nals& nals) {
        const Bytes oldSps=spsBytes, oldPps=ppsBytes;
        update(nals);
        AccessUnit a;
        a.parameterChange = oldSps != spsBytes || oldPps != ppsBytes;
        for (const auto& n : nals) {
            unsigned t = n[0]&31;
            a.idr |= t==5; a.slices |= t==1 || t==5;
            if (sliceIsB(n)) throw Error("decoder_format", "B frames violate low latency protocol");
        }
        Nals out;
        if (a.idr) {
            if (spsBytes.empty() || ppsBytes.empty()) throw Error("decoder_format", "IDR lacks SPS/PPS");
            out={spsBytes, ppsBytes};
        }
        for (const auto& n : nals) if (!a.idr || ((n[0]&31)!=7 && (n[0]&31)!=8)) out.push_back(n);
        a.annexB=join(out);
        return a;
    }
    std::string profileLevelId() const {
        if (spsBytes.size()<4) throw Error("encoder_format", "SPS missing");
        const char* h="0123456789abcdef"; std::string r;
        for (unsigned i=1; i<=3; ++i) { r+=h[spsBytes[i]>>4]; r+=h[spsBytes[i]&15]; }
        return r;
    }
};
} // namespace wire
