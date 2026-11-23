// SPDX-License-Identifier: MIT
#include "protocol_core.h"
#include "decoder_fixture.h"
#include <iostream>
#include <random>
using namespace wire;
int checks=0;
void require(bool condition,const char* message){++checks;if(!condition)throw std::runtime_error(message);}
template<class F>void rejects(F f,const char* message){bool caught=false;try{f();}catch(const Error&){caught=true;}require(caught,message);}
int main(){try{
    Bytes be;be32(be,0x12345678);be64(be,0x0123456789abcdefull);
    require(readBE(be.data(),4)==0x12345678,"BE32");require(readBE(be.data()+4,8)==0x0123456789abcdefull,"BE64");
    require(rgbaSize(320,180)==230400,"tight RGBA size");
    rejects([]{rgbaSize(8192,8192);},"64 MiB RGBA guard");rejects([]{rgbaSize(0,32);},"zero dimension");
    Control c;require(control("{\"action\":\"keyframe\"}\r",c)&&c.action=="keyframe","keyframe control");
    require(control("{ \"bitrate\":3000000, \"action\":\"bitrate\" }",c)&&c.bitrate==3000000,"reordered bitrate");
    require(control("{\"action\":\"stop\"}",c),"stop control");
    for(const auto* bad:{"","{}","{\"action\":\"stop\",}","{\"action\":\"stop\"}x","{\"action\":\"bitrate\",\"bitrate\":99999}","{\"action\":\"bitrate\",\"bitrate\":3000000000}","{\"action\":\"bitrate\",\"bitrate\":0100000}","{\"action\":\"stop\",\"action\":\"stop\"}","{\"action\":\"stop\",\"bitrate\":100000}"})require(!control(bad,c),"invalid JSON control accepted");
    require(!control(std::string(5000,'x'),c),"control length bound");
    Bytes fixture(std::begin(DecoderFixture),std::end(DecoderFixture));auto nals=annex(fixture);
    require(nals.size()==3,"fixture NAL count");require((nals[0][0]&31)==7&&(nals[1][0]&31)==8&&(nals[2][0]&31)==5,"fixture SPS PPS IDR");
    auto dim=sps(nals[0]);require(dim.width==320&&dim.height==180&&dim.codedHeight==192,"SPS display crop");
    require(dim.left==0&&dim.top==0,"crop origin");
    require(annex(join(nals))==nals,"Annex-B round trip");
    Bytes avcc={1,nals[0][1],nals[0][2],nals[0][3],0xff,0xe1};
    avcc.push_back(uint8_t(nals[0].size()>>8));avcc.push_back(uint8_t(nals[0].size()));avcc.insert(avcc.end(),nals[0].begin(),nals[0].end());avcc.push_back(1);
    avcc.push_back(uint8_t(nals[1].size()>>8));avcc.push_back(uint8_t(nals[1].size()));avcc.insert(avcc.end(),nals[1].begin(),nals[1].end());
    unsigned lengthSize=0;auto config=sequenceHeader(avcc,lengthSize);require(lengthSize==4&&config.size()==2,"avcC sequence header");
    for(unsigned length:{1u,2u,3u,4u}){Bytes b;for(const auto& n:nals){for(int shift=int(length-1)*8;shift>=0;shift-=8)b.push_back(uint8_t(n.size()>>shift));b.insert(b.end(),n.begin(),n.end());}require(avc(b,length)==nals,"AVC length round trip");}
    ParameterSets sets;auto au=sets.au(nals);require(au.idr&&au.slices&&au.parameterChange,"IDR assembly");
    auto idr=sets.au({nals[2]});require(idr.idr&&annex(idr.annexB).size()==3,"IDR parameter-set repetition");
    require(sets.profileLevelId().size()==6,"profile level id from SPS");
    require(sliceIsB(Bytes{0x41,0xa0}),"B slice detection");require(!sliceIsB(Bytes{0x41,0xe0}),"P slice detection");
    rejects([&]{sets.au({Bytes{0x41,0xa0}});},"B frame rejection");
    rejects([&]{ParameterSets empty;empty.au({nals[2]});},"IDR without parameter sets");
    rejects([]{annex(Bytes{0,0,1});},"empty Annex-B NAL");rejects([]{annex(Bytes{7,0,0,1,0x65});},"leading garbage");
    rejects([]{avc(Bytes{0,0,0,4,0x65},4);},"truncated AVC NAL");rejects([]{avc(Bytes{0},0);},"bad AVC length size");
    rejects([]{unsigned length=4;sequenceHeader(Bytes{1,2,3},length);},"truncated sequence header");
    std::mt19937 random(0x56414e54u);for(unsigned k=0;k<10000;++k){Bytes b(size_t(random()%128));for(auto& x:b)x=uint8_t(random());try{auto ns=annex(b);for(auto& n:ns)if((n[0]&31)==7)sps(n);}catch(const Error&){}++checks;}
    for(unsigned k=0;k<10000;++k){Bytes b=nals[0];b[size_t(random()%b.size())]^=uint8_t(1u<<(random()%8));try{sps(b);}catch(const Error&){}++checks;}
    for(size_t k=0;k<std::min<size_t>(8,nals[0].size());++k)rejects([&]{sps(Bytes(nals[0].begin(),nals[0].begin()+k));},"truncated SPS dimensions");
    std::cout<<"PASS protocol core checks="<<checks<<"\n";return 0;
}catch(const std::exception& e){std::cerr<<"FAIL "<<e.what()<<"\n";return 1;}}
