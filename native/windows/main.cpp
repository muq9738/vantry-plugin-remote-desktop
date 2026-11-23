// SPDX-License-Identifier: MIT
// Windows 10+ media-only helper. No input injection, permission UI or software codec fallback.
#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <windows.h>
#include <wtsapi32.h>
#include <d3d11.h>
#include <d3d10.h>
#include <dxgi1_2.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mferror.h>
#include <mftransform.h>
#include <wmcodecdsp.h>
#include <strmif.h>
#include <codecapi.h>
#include <wrl/client.h>
#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdio>
#include <cstring>
#include <deque>
#include <functional>
#include <exception>
#include <iomanip>
#include <map>
#include <memory>
#include <mutex>
#include <sstream>
#include <thread>
#include "protocol_core.h"
#include "decoder_fixture.h"

using Microsoft::WRL::ComPtr;
using wire::Bytes;
using wire::Error;
using wire::Record;
using Clock = std::chrono::steady_clock;
using namespace std::chrono_literals;
namespace {
std::string hex32(uint32_t v) {
    std::ostringstream s; s << "0x" << std::hex << std::setw(8) << std::setfill('0') << v; return s.str();
}
void check(HRESULT hr, const char* step, const char* code = "internal", int exitCode = 1) {
    if (FAILED(hr)) throw Error(code, std::string(step)+" failed ("+hex32(uint32_t(hr))+")", exitCode);
}
void log(const std::string& message) {
    static std::mutex lock; std::lock_guard<std::mutex> guard(lock);
    std::fprintf(stderr, "[vantry-native %lu] %s\n", GetCurrentProcessId(), message.c_str());
}
struct State {
    std::atomic<bool> stop{false}, forceIDR{true}, ioFailed{false};
    std::atomic<uint32_t> newBitrate{0};
    std::atomic<ULONGLONG> shutdownAt{0}, cleanupAt{0}, startupDeadline{0};
    void shutdown() { ULONGLONG zero=0; shutdownAt.compare_exchange_strong(zero, GetTickCount64()); }
};
// Declared after live resource owners, so fatal unwind arms the shutdown deadline before COM release.
struct TerminalUnwind {
    State& state;int exceptions=std::uncaught_exceptions();
    explicit TerminalUnwind(State& s):state(s){}
    ~TerminalUnwind(){if(std::uncaught_exceptions()>exceptions)state.shutdown();}
};
struct CleanupWatch {
    State& state;ULONGLONG previous;
    explicit CleanupWatch(State& s):state(s),previous(s.cleanupAt.exchange(GetTickCount64())){}
    ~CleanupWatch(){state.cleanupAt=previous;}
};
std::atomic<State*> signalState{nullptr};
BOOL WINAPI consoleHandler(DWORD event) {
    if (event==CTRL_C_EVENT || event==CTRL_BREAK_EVENT || event==CTRL_CLOSE_EVENT || event==CTRL_SHUTDOWN_EVENT) {
        if (State* state=signalState.load()) { state->shutdown();state->stop=true; }
        return TRUE;
    }
    return FALSE;
}
struct Watchdog {
    State& state;
    std::atomic<bool> done{false};
    std::thread worker;
    explicit Watchdog(State& s) : state(s), worker([this] {
        while (!done) {
            ULONGLONG start=state.shutdownAt.load(),cleanup=state.cleanupAt.load();
            if ((start && GetTickCount64()-start > 4000)||(cleanup && GetTickCount64()-cleanup > 4000)) {
                // Fatal termination cannot wait on stderr or another thread's
                // log mutex: a full diagnostic pipe must not defeat shutdown.
                TerminateProcess(GetCurrentProcess(),70);
            }
            const ULONGLONG deadline=state.startupDeadline.load();
            if(deadline&&GetTickCount64()>deadline){TerminateProcess(GetCurrentProcess(),70);}
            Sleep(20);
        }
    }) {}
    ~Watchdog() { done=true; worker.join(); }
};
struct Runtime {
    State& state;
    explicit Runtime(State& s):state(s) {
        check(CoInitializeEx(nullptr, COINIT_MULTITHREADED), "COM startup");
        HRESULT hr=MFStartup(MF_VERSION, MFSTARTUP_FULL);
        if (FAILED(hr)) { CoUninitialize(); check(hr, "MF startup"); }
    }
    ~Runtime() { CleanupWatch cleanup(state);MFShutdown();CoUninitialize(); }
};
struct Timebase {
    int64_t frequency=0, qpc=0;
    uint64_t unixUs=0;
    Timebase() {
        LARGE_INTEGER f{}, a{}, b{}; FILETIME ft{};
        QueryPerformanceFrequency(&f); QueryPerformanceCounter(&a);
        GetSystemTimePreciseAsFileTime(&ft); QueryPerformanceCounter(&b);
        frequency=f.QuadPart; qpc=a.QuadPart+(b.QuadPart-a.QuadPart)/2;
        const uint64_t ticks=(uint64_t(ft.dwHighDateTime)<<32)|ft.dwLowDateTime;
        unixUs=(ticks-116444736000000000ull)/10;
        if (frequency<=0) throw Error("internal", "QPC is unavailable");
    }
    int64_t now() const { LARGE_INTEGER x{}; QueryPerformanceCounter(&x); return x.QuadPart; }
    uint64_t micros(int64_t value) const {
        const int64_t delta=value-qpc;
        const int64_t us=(delta/frequency)*1000000+(delta%frequency)*1000000/frequency;
        return uint64_t(int64_t(unixUs)+us);
    }
};

// One writer owns stdout. Media queues are bounded by count AND 128 MiB total.
// A full H264 queue resets the chain and requests an IDR; reference P frames are never dropped alone.
class Writer {
    State& state_;
    HANDLE out_=GetStdHandle(STD_OUTPUT_HANDLE);
    std::mutex mutex_;
    std::condition_variable cv_;
    std::deque<Record> queue_;
    size_t mediaLimit_, bytes_=0;
    bool closing_=false, done_=false, awaitingIDR_=false;
    std::thread worker_;
    static constexpr size_t ByteLimit=128u*1024u*1024u;
    bool writeAll(const uint8_t* p, size_t n) {
        while (n) {
            DWORD written=0;
            if (!WriteFile(out_, p, DWORD(std::min<size_t>(n, 1u<<20)), &written, nullptr) || !written) return false;
            p+=written; n-=written;
        }
        return true;
    }
    size_t mediaCount() const {
        return size_t(std::count_if(queue_.begin(), queue_.end(), [](const Record& r){ return r.type==2 || r.type==3; }));
    }
    void eraseMedia() {
        for (auto it=queue_.begin(); it!=queue_.end();) {
            if (it->type==2 || it->type==3) { bytes_-=it->payload.size(); it=queue_.erase(it); }
            else ++it;
        }
    }
    void run() {
        for (;;) {
            Record r;
            {
                std::unique_lock<std::mutex> lock(mutex_);
                cv_.wait(lock,[&]{ return closing_ || !queue_.empty(); });
                if (queue_.empty()) break;
                r=std::move(queue_.front()); queue_.pop_front(); bytes_-=r.payload.size(); cv_.notify_all();
            }
            const uint32_t n=uint32_t(r.payload.size());
            const uint8_t header[]={r.type,uint8_t(n>>24),uint8_t(n>>16),uint8_t(n>>8),uint8_t(n)};
            if (!writeAll(header,sizeof(header)) || !writeAll(r.payload.data(),r.payload.size())) {
                state_.ioFailed=true; state_.stop=true; state_.shutdown(); break;
            }
        }
        std::lock_guard<std::mutex> lock(mutex_); done_=true; cv_.notify_all();
    }
public:
    std::atomic<uint64_t> dropped{0};
    Writer(State& s, size_t limit) : state_(s), mediaLimit_(limit), worker_([this]{run();}) {}
    ~Writer() { finish(); }
    void control(Record r) {
        if (r.payload.size()>wire::MaxPayload) throw Error("payload_too_large","Control payload too large",6);
        std::lock_guard<std::mutex> lock(mutex_);
        if (done_ || closing_) return;
        // Events are best effort and capped; hello/error are never dropped.
        if (r.type==4 && queue_.size()>=mediaLimit_+8) return;
        if (r.type==127) { eraseMedia(); }
        bytes_+=r.payload.size(); queue_.push_back(std::move(r)); cv_.notify_all();
    }
    bool media(Record r, bool idr=false, bool block=false) {
        if (r.payload.size()>wire::MaxPayload) throw Error("payload_too_large","Media payload too large",6);
        std::unique_lock<std::mutex> lock(mutex_);
        auto full=[&]{ return mediaCount()>=mediaLimit_ || bytes_+r.payload.size()>ByteLimit; };
        if (r.type==2) {
            if (full()) { eraseMedia(); awaitingIDR_=true; state_.forceIDR=true; ++dropped; }
            if (awaitingIDR_ && !idr) { ++dropped; return false; }
            if (idr) awaitingIDR_=false;
        } else if (block) {
            while (full() && !done_ && !closing_ && !state_.stop) cv_.wait_for(lock,10ms);
        } else {
            while (full()) {
                auto it=std::find_if(queue_.begin(),queue_.end(),[](const Record& x){return x.type==3;});
                if (it==queue_.end()) break;
                bytes_-=it->payload.size(); queue_.erase(it); ++dropped;
            }
        }
        if (done_ || closing_ || (block && state_.stop)) return false;
        bytes_+=r.payload.size(); queue_.push_back(std::move(r)); cv_.notify_all(); return true;
    }
    void finish() {
        if (!worker_.joinable()) return;
        CleanupWatch cleanup(state_);
        {
            std::unique_lock<std::mutex> lock(mutex_); closing_=true; cv_.notify_all();
            if (!cv_.wait_for(lock,2s,[&]{return done_;})) {
                state_.shutdown();state_.stop=true;
                while(!done_) {
                    CancelSynchronousIo(reinterpret_cast<HANDLE>(worker_.native_handle()));
                    cv_.wait_for(lock,10ms);
                }
            }
        }
        worker_.join();
    }
};

// Cancellable synchronous stdin reader. Capture EOF stops; finite pattern ignores EOF.
// Decode has a separate two-record queue, so MF output can continue while stdin is idle.
class Reader {
    State& state_;
    bool binary_, stopOnEOF_;
    HANDLE input_=GetStdHandle(STD_INPUT_HANDLE);
    std::atomic<bool> cancel_{false};
    std::mutex mutex_;
    std::condition_variable cv_;
    std::deque<Record> queue_;
    bool eof_=false, done_=false;
    std::unique_ptr<Error> failure_;
    std::thread worker_;
    bool exact(uint8_t* p, size_t n, bool allowEOF=false) {
        size_t offset=0;
        while (offset<n && !cancel_) {
            DWORD got=0;
            BOOL ok=ReadFile(input_,p+offset,DWORD(std::min<size_t>(n-offset,1u<<20)),&got,nullptr);
            if (!ok) {
                DWORD e=GetLastError();
                if (e==ERROR_OPERATION_ABORTED && cancel_) return false;
                if (e!=ERROR_BROKEN_PIPE && e!=ERROR_HANDLE_EOF) throw Error("input_failed","stdin read failed",6);
                got=0;
            }
            if (!got) {
                if (!offset && allowEOF) return false;
                throw Error("input_truncated","stdin record is truncated",6);
            }
            offset+=got;
        }
        return offset==n;
    }
    void runBinary() {
        while (!cancel_) {
            uint8_t h[5]{};
            if (!exact(h,5,true)) break;
            const uint32_t n=uint32_t(wire::readBE(h+1,4));
            if (n>wire::MaxPayload) throw Error("payload_too_large","stdin payload exceeds 64 MiB",6);
            Record r{h[0],Bytes(n)};
            if (n && !exact(r.payload.data(),n)) break;
            std::unique_lock<std::mutex> lock(mutex_);
            while (queue_.size()>=2 && !cancel_) cv_.wait_for(lock,10ms);
            if (cancel_) break;
            queue_.push_back(std::move(r)); cv_.notify_all();
        }
    }
    void runControl() {
        std::string line; bool overflow=false;
        uint8_t chunk[1024];
        while (!cancel_) {
            DWORD got=0;
            if (!ReadFile(input_,chunk,sizeof(chunk),&got,nullptr) || !got) break;
            for (DWORD i=0;i<got;++i) {
                const char c=char(chunk[i]);
                if (c=='\n') {
                    wire::Control control;
                    if (!overflow && wire::control(line,control)) {
                        if (control.action=="stop") { state_.stop=true; state_.shutdown(); }
                        else if (control.action=="keyframe") state_.forceIDR=true;
                        else if (control.action=="bitrate") state_.newBitrate=control.bitrate;
                    } else log("invalid_control_ignored");
                    line.clear(); overflow=false;
                } else if (line.size()<4096 && !overflow) line+=c;
                else { line.clear(); overflow=true; }
            }
        }
    }
    void run() {
        try { if (binary_) runBinary(); else runControl(); }
        catch (const Error& e) { std::lock_guard<std::mutex> lock(mutex_); failure_=std::make_unique<Error>(e); }
        catch (...) { std::lock_guard<std::mutex> lock(mutex_); failure_=std::make_unique<Error>("internal","stdin worker failed"); }
        {
            std::lock_guard<std::mutex> lock(mutex_); eof_=true; done_=true; cv_.notify_all();
        }
        if(!cancel_&&binary_)state_.shutdown(); // EOF starts the drain deadline even if media is waiting inside a driver.
        if (!cancel_ && stopOnEOF_) { state_.stop=true; state_.shutdown(); }
    }
public:
    Reader(State& state, bool binary, bool stopOnEOF)
        : state_(state),binary_(binary),stopOnEOF_(stopOnEOF),worker_([this]{run();}) {}
    ~Reader() { close(); }
    bool pop(Record& r, bool& eof) {
        std::unique_lock<std::mutex> lock(mutex_);
        if (failure_) throw *failure_;
        if (queue_.empty() && !eof_) cv_.wait_for(lock,2ms);
        if (failure_) throw *failure_;
        eof=eof_ && queue_.empty();
        if (queue_.empty()) return false;
        r=std::move(queue_.front()); queue_.pop_front(); cv_.notify_all(); return true;
    }
    void close() {
        if (!worker_.joinable()) return;
        CleanupWatch cleanup(state_);cancel_=true;cv_.notify_all();
        // Repeated cancellation closes the race between the flag check and entering ReadFile.
        std::unique_lock<std::mutex> lock(mutex_);
        while (!done_) {
            CancelSynchronousIo(reinterpret_cast<HANDLE>(worker_.native_handle()));
            cv_.wait_for(lock,10ms);
        }
        lock.unlock(); worker_.join();
    }
};

struct Options {
    std::string mode, policy="drop-oldest", rate="lowlatency";
    uint32_t width=1920,height=1080,fps=30,bitrate=6000000,display=0,queue=6;
    uint64_t frames=120;
    double keyInterval=2.0;
    bool noPace=false,noise=false,noCursor=false,events=false;
};
uint64_t number(const std::string& s,uint64_t min,uint64_t max) {
    if (s.empty()) throw Error("usage","Missing integer value",64);
    uint64_t v=0;
    for (char c:s) {
        if (c<'0'||c>'9'||v>(max-uint64_t(c-'0'))/10) throw Error("usage","Integer value is out of range",64);
        v=v*10+uint64_t(c-'0');
    }
    if (v<min||v>max) throw Error("usage","Integer value is out of range",64);
    return v;
}
Options options(int argc,char** argv) {
    if (argc<2) throw Error("usage","Expected probe, pattern, capture or decode",64);
    Options o; o.mode=argv[1];
    if (o.mode!="probe"&&o.mode!="pattern"&&o.mode!="capture"&&o.mode!="decode") throw Error("usage","Unknown mode",64);
    if (o.mode=="pattern") {o.width=1280;o.height=720;}
    if (o.mode=="decode") o.queue=2;
    std::vector<std::string> seen;
    for (int i=2;i<argc;++i) {
        std::string key=argv[i];
        if (std::find(seen.begin(),seen.end(),key)!=seen.end()) throw Error("usage","Duplicate option",64);
        seen.push_back(key);
        if (key=="--events"&&o.mode!="probe") {o.events=true;continue;}
        if (key=="--no-cursor"&&o.mode=="capture") {o.noCursor=true;continue;}
        if (key=="--no-pace"&&o.mode=="pattern") {o.noPace=true;continue;}
        if (key=="--noise"&&o.mode=="pattern") {o.noise=true;continue;}
        // Accepted as a host compatibility flag; Windows never prompts for a global TCC permission.
        if (key=="--allow-prompt"&&o.mode=="capture") continue;
        if (o.mode=="probe"||++i>=argc) throw Error("usage","Unsupported or incomplete option",64);
        std::string value=argv[i];
        bool encode=o.mode=="pattern"||o.mode=="capture";
        if (key=="--queue") o.queue=uint32_t(number(value,1,32));
        else if (key=="--output-policy"&&o.mode=="decode") {
            if(value!="block"&&value!="drop-oldest")throw Error("usage","Invalid output policy",64);o.policy=value;
        } else if (key=="--display"&&o.mode=="capture") o.display=uint32_t(number(value,0,UINT32_MAX));
        else if (key=="--frames"&&o.mode=="pattern") o.frames=number(value,0,UINT32_MAX);
        else if (key=="--width"&&encode) o.width=uint32_t(number(value,64,8192));
        else if (key=="--height"&&encode) o.height=uint32_t(number(value,64,8192));
        else if (key=="--fps"&&encode) o.fps=uint32_t(number(value,1,120));
        else if (key=="--bitrate"&&encode) o.bitrate=uint32_t(number(value,100000,200000000));
        else if (key=="--rate-control"&&encode) {
            if(value!="standard"&&value!="lowlatency")throw Error("usage","Invalid rate control",64);o.rate=value;
        } else if (key=="--keyframe-interval"&&encode) {
            size_t used=0;
            try {o.keyInterval=std::stod(value,&used);}catch(...){throw Error("usage","Invalid keyframe interval",64);}
            if (used!=value.size()||!(o.keyInterval>=0.1&&o.keyInterval<=3600))throw Error("usage","Invalid keyframe interval",64);
        } else throw Error("usage","Unsupported option",64);
    }
    if ((o.width&1)||(o.height&1)) throw Error("usage","Dimensions must be even",64);
    wire::rgbaSize(o.width,o.height);
    return o;
}
std::string guidString(const GUID& guid) {
    wchar_t w[40]{}; StringFromGUID2(guid,w,40);
    std::string s; for (const wchar_t* p=w;*p;++p) s+=char(*p); return s;
}
struct Display {
    uint32_t id=0,width=0,height=0;
    bool main=false;
    DXGI_OUTPUT_DESC desc{};
    ComPtr<IDXGIAdapter1> adapter;
    ComPtr<IDXGIOutput1> output;
};
std::vector<ComPtr<IDXGIAdapter1>> adapters() {
    ComPtr<IDXGIFactory1> factory; check(CreateDXGIFactory1(IID_PPV_ARGS(&factory)),"DXGI factory");
    std::vector<ComPtr<IDXGIAdapter1>> list;
    for (UINT i=0;;++i) {
        ComPtr<IDXGIAdapter1> a; HRESULT hr=factory->EnumAdapters1(i,&a);
        if(hr==DXGI_ERROR_NOT_FOUND)break;check(hr,"DXGI adapter enumeration");
        DXGI_ADAPTER_DESC1 d{};check(a->GetDesc1(&d),"DXGI adapter description");
        if (!(d.Flags&DXGI_ADAPTER_FLAG_SOFTWARE)) list.push_back(a);
    }
    return list;
}
std::vector<Display> displays() {
    std::vector<Display> list;
    for (auto& a:adapters()) {
        for(UINT i=0;;++i) {
            ComPtr<IDXGIOutput> output;HRESULT hr=a->EnumOutputs(i,&output);
            if(hr==DXGI_ERROR_NOT_FOUND)break;check(hr,"DXGI output enumeration");
            Display d;check(output->GetDesc(&d.desc),"DXGI output description");
            if(!d.desc.AttachedToDesktop)continue;
            check(output.As(&d.output),"DXGI output1");d.adapter=a;
            d.width=uint32_t(d.desc.DesktopCoordinates.right-d.desc.DesktopCoordinates.left);
            d.height=uint32_t(d.desc.DesktopCoordinates.bottom-d.desc.DesktopCoordinates.top);
            MONITORINFO mi{};mi.cbSize=sizeof(mi);if(GetMonitorInfoW(d.desc.Monitor,&mi))d.main=(mi.dwFlags&MONITORINFOF_PRIMARY)!=0;
            // Match the host's GetMonitorInfoW device-name mapping, never enumeration order.
            const std::wstring name=d.desc.DeviceName, prefix=L"\\\\.\\DISPLAY";
            if(name.compare(0,prefix.size(),prefix)!=0 || name.size()==prefix.size())
                throw Error("display_id_unmapped","DXGI device name has no DISPLAY numeric ID",4);
            uint64_t id=0;
            for(size_t k=prefix.size();k<name.size();++k) {
                if(name[k]<L'0'||name[k]>L'9'||id>(UINT32_MAX-uint64_t(name[k]-L'0'))/10)
                    throw Error("display_id_unmapped","DXGI device name has an invalid DISPLAY numeric ID",4);
                id=id*10+uint64_t(name[k]-L'0');
            }
            if(!id)throw Error("display_id_unmapped","DISPLAY zero conflicts with the primary-display alias",4);
            d.id=uint32_t(id);
            if(std::any_of(list.begin(),list.end(),[&](const Display& x){return x.id==d.id;}))
                throw Error("display_id_unmapped","DXGI DISPLAY numeric IDs are ambiguous",4);
            list.push_back(std::move(d));
        }
    }
    return list;
}
struct Gpu {
    ComPtr<IDXGIAdapter1> adapter;
    ComPtr<ID3D11Device> device;
    ComPtr<ID3D11DeviceContext> context;
    ComPtr<ID3D11VideoDevice> video;
    ComPtr<ID3D11VideoContext> videoContext;
    ComPtr<IMFDXGIDeviceManager> manager;
    DXGI_ADAPTER_DESC1 desc{};
    explicit Gpu(ComPtr<IDXGIAdapter1> a) : adapter(a) {
        check(adapter->GetDesc1(&desc),"Hardware adapter description");
        if(desc.Flags&DXGI_ADAPTER_FLAG_SOFTWARE)throw Error("hw_encoder_unavailable","Software adapter rejected",2);
        D3D_FEATURE_LEVEL level{};D3D_FEATURE_LEVEL levels[]={D3D_FEATURE_LEVEL_11_1,D3D_FEATURE_LEVEL_11_0};
        check(D3D11CreateDevice(adapter.Get(),D3D_DRIVER_TYPE_UNKNOWN,nullptr,D3D11_CREATE_DEVICE_BGRA_SUPPORT|D3D11_CREATE_DEVICE_VIDEO_SUPPORT,
            levels,2,D3D11_SDK_VERSION,&device,&level,&context),"Hardware D3D11 device","hw_encoder_unavailable",2);
        check(device.As(&video),"D3D11 video device");check(context.As(&videoContext),"D3D11 video context");
        ComPtr<ID3D10Multithread> multi;check(device.As(&multi),"D3D multithread protection");multi->SetMultithreadProtected(TRUE);
        UINT token=0;check(MFCreateDXGIDeviceManager(&token,&manager),"DXGI device manager");
        check(manager->ResetDevice(device.Get(),token),"DXGI manager device");
    }
    std::string luid() const { return hex32(uint32_t(desc.AdapterLuid.HighPart))+":"+hex32(desc.AdapterLuid.LowPart); }
    ComPtr<ID3D11Texture2D> texture(UINT w,UINT h,DXGI_FORMAT format,UINT bind=D3D11_BIND_RENDER_TARGET) {
        D3D11_TEXTURE2D_DESC d{};d.Width=w;d.Height=h;d.MipLevels=1;d.ArraySize=1;
        d.Format=format;d.SampleDesc.Count=1;d.Usage=D3D11_USAGE_DEFAULT;d.BindFlags=bind;
        ComPtr<ID3D11Texture2D> t;check(device->CreateTexture2D(&d,nullptr,&t),"D3D11 texture");return t;
    }
};
class Converter {
    Gpu& gpu_;
    ComPtr<ID3D11VideoProcessorEnumerator> enumerator_;
    ComPtr<ID3D11VideoProcessor> processor_;
    UINT iw_=0,ih_=0,ow_=0,oh_=0;
    DXGI_FORMAT input_=DXGI_FORMAT_UNKNOWN,output_=DXGI_FORMAT_UNKNOWN;
public:
    explicit Converter(Gpu& g):gpu_(g){}
    ComPtr<ID3D11Texture2D> process(ID3D11Texture2D* source,UINT subresource,UINT outW,UINT outH,
        DXGI_FORMAT outFormat,RECT crop,D3D11_VIDEO_PROCESSOR_ROTATION rotation=D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY) {
        D3D11_TEXTURE2D_DESC d{};source->GetDesc(&d);
        if(crop.left<0||crop.top<0||crop.right>LONG(d.Width)||crop.bottom>LONG(d.Height)||crop.right<=crop.left||crop.bottom<=crop.top)
            throw Error("decoder_format","Video surface crop is out of bounds");
        if(!processor_||iw_!=d.Width||ih_!=d.Height||ow_!=outW||oh_!=outH||input_!=d.Format||output_!=outFormat) {
            processor_.Reset();enumerator_.Reset();
            D3D11_VIDEO_PROCESSOR_CONTENT_DESC c{};c.InputFrameFormat=D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
            c.InputWidth=d.Width;c.InputHeight=d.Height;c.OutputWidth=outW;c.OutputHeight=outH;
            c.InputFrameRate={30,1};c.OutputFrameRate={30,1};c.Usage=D3D11_VIDEO_USAGE_PLAYBACK_NORMAL;
            check(gpu_.video->CreateVideoProcessorEnumerator(&c,&enumerator_),"Video processor enumerator");
            UINT inSupport=0,outSupport=0;
            check(enumerator_->CheckVideoProcessorFormat(d.Format,&inSupport),"Video input format");
            check(enumerator_->CheckVideoProcessorFormat(outFormat,&outSupport),"Video output format");
            if(!(inSupport&D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_INPUT)||!(outSupport&D3D11_VIDEO_PROCESSOR_FORMAT_SUPPORT_OUTPUT))
                throw Error("video_processor_unavailable","Hardware video processor cannot convert these formats",2);
            D3D11_VIDEO_PROCESSOR_CAPS caps{};check(enumerator_->GetVideoProcessorCaps(&caps),"Video processor caps");
            if(rotation!=D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY && !(caps.FeatureCaps&D3D11_VIDEO_PROCESSOR_FEATURE_CAPS_ROTATION))
                throw Error("capture_unavailable","Hardware video processor cannot rotate this display",4);
            check(gpu_.video->CreateVideoProcessor(enumerator_.Get(),0,&processor_),"Video processor");
            iw_=d.Width;ih_=d.Height;ow_=outW;oh_=outH;input_=d.Format;output_=outFormat;
        }
        ComPtr<ID3D11Texture2D> target=gpu_.texture(outW,outH,outFormat);
        D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC in{};in.ViewDimension=D3D11_VPIV_DIMENSION_TEXTURE2D;
        in.Texture2D.MipSlice=subresource%d.MipLevels;in.Texture2D.ArraySlice=subresource/d.MipLevels;
        ComPtr<ID3D11VideoProcessorInputView> inputView;
        check(gpu_.video->CreateVideoProcessorInputView(source,enumerator_.Get(),&in,&inputView),"Video input view");
        D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC out{};out.ViewDimension=D3D11_VPOV_DIMENSION_TEXTURE2D;out.Texture2D.MipSlice=0;
        ComPtr<ID3D11VideoProcessorOutputView> outputView;
        check(gpu_.video->CreateVideoProcessorOutputView(target.Get(),enumerator_.Get(),&out,&outputView),"Video output view");
        RECT dest{0,0,LONG(outW),LONG(outH)};
        gpu_.videoContext->VideoProcessorSetStreamFrameFormat(processor_.Get(),0,D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE);
        gpu_.videoContext->VideoProcessorSetStreamAutoProcessingMode(processor_.Get(),0,FALSE);
        gpu_.videoContext->VideoProcessorSetStreamSourceRect(processor_.Get(),0,TRUE,&crop);
        gpu_.videoContext->VideoProcessorSetStreamDestRect(processor_.Get(),0,TRUE,&dest);
        gpu_.videoContext->VideoProcessorSetOutputTargetRect(processor_.Get(),TRUE,&dest);
        gpu_.videoContext->VideoProcessorSetStreamRotation(processor_.Get(),0,rotation!=D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY,rotation);
        D3D11_VIDEO_PROCESSOR_COLOR_SPACE inputColor{},outputColor{};
        inputColor.YCbCr_Matrix=1;outputColor.YCbCr_Matrix=1; // BT.709
        inputColor.Nominal_Range=d.Format==DXGI_FORMAT_NV12?D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235:D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_0_255;
        outputColor.Nominal_Range=outFormat==DXGI_FORMAT_NV12?D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235:D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_0_255;
        gpu_.videoContext->VideoProcessorSetStreamColorSpace(processor_.Get(),0,&inputColor);
        gpu_.videoContext->VideoProcessorSetOutputColorSpace(processor_.Get(),&outputColor);
        D3D11_VIDEO_PROCESSOR_STREAM stream{};stream.Enable=TRUE;stream.pInputSurface=inputView.Get();
        check(gpu_.videoContext->VideoProcessorBlt(processor_.Get(),outputView.Get(),0,1,&stream),"Video processor conversion");
        return target;
    }
};
Bytes readBGRA(Gpu& gpu,ID3D11Texture2D* texture) {
    D3D11_TEXTURE2D_DESC d{};texture->GetDesc(&d);
    if(d.Format!=DXGI_FORMAT_B8G8R8A8_UNORM||d.ArraySize!=1||d.MipLevels!=1)throw Error("internal","Unexpected BGRA readback surface");
    Bytes result(wire::rgbaSize(d.Width,d.Height));
    d.Usage=D3D11_USAGE_STAGING;d.BindFlags=0;d.CPUAccessFlags=D3D11_CPU_ACCESS_READ;d.MiscFlags=0;
    ComPtr<ID3D11Texture2D> staging;check(gpu.device->CreateTexture2D(&d,nullptr,&staging),"Readback staging texture");
    gpu.context->CopyResource(staging.Get(),texture);
    D3D11_MAPPED_SUBRESOURCE map{};check(gpu.context->Map(staging.Get(),0,D3D11_MAP_READ,0,&map),"GPU readback map");
    if(map.RowPitch<d.Width*4) {gpu.context->Unmap(staging.Get(),0);throw Error("internal","Invalid BGRA row pitch");}
    for(UINT y=0;y<d.Height;++y)std::memcpy(result.data()+size_t(y)*d.Width*4,static_cast<uint8_t*>(map.pData)+size_t(y)*map.RowPitch,size_t(d.Width)*4);
    gpu.context->Unmap(staging.Get(),0);return result;
}
ComPtr<IMFMediaType> videoType(const GUID& subtype,UINT width,UINT height,UINT fps) {
    ComPtr<IMFMediaType> t;check(MFCreateMediaType(&t),"Media type");
    check(t->SetGUID(MF_MT_MAJOR_TYPE,MFMediaType_Video),"Video major type");
    check(t->SetGUID(MF_MT_SUBTYPE,subtype),"Video subtype");
    check(MFSetAttributeSize(t.Get(),MF_MT_FRAME_SIZE,width,height),"Video frame size");
    check(MFSetAttributeRatio(t.Get(),MF_MT_FRAME_RATE,fps,1),"Video frame rate");
    check(MFSetAttributeRatio(t.Get(),MF_MT_PIXEL_ASPECT_RATIO,1,1),"Video pixel aspect ratio");
    check(t->SetUINT32(MF_MT_INTERLACE_MODE,MFVideoInterlace_Progressive),"Progressive video");
    check(t->SetUINT32(MF_MT_VIDEO_PRIMARIES,MFVideoPrimaries_BT709),"Video primaries");
    check(t->SetUINT32(MF_MT_TRANSFER_FUNCTION,MFVideoTransFunc_709),"Video transfer function");
    check(t->SetUINT32(MF_MT_YUV_MATRIX,MFVideoTransferMatrix_BT709),"Video matrix");
    check(t->SetUINT32(MF_MT_VIDEO_NOMINAL_RANGE,MFNominalRange_16_235),"Video nominal range");
    return t;
}
ComPtr<IMFSample> byteSample(const Bytes& bytes,LONGLONG pts,LONGLONG duration) {
    if(bytes.empty()||bytes.size()>wire::MaxPayload)throw Error("payload_too_large","Invalid compressed sample size",6);
    ComPtr<IMFMediaBuffer> buffer;check(MFCreateMemoryBuffer(DWORD(bytes.size()),&buffer),"Sample memory buffer");
    BYTE* p=nullptr;check(buffer->Lock(&p,nullptr,nullptr),"Sample buffer lock");std::memcpy(p,bytes.data(),bytes.size());
    check(buffer->Unlock(),"Sample buffer unlock");check(buffer->SetCurrentLength(DWORD(bytes.size())),"Sample buffer length");
    ComPtr<IMFSample> sample;check(MFCreateSample(&sample),"Media sample");check(sample->AddBuffer(buffer.Get()),"Sample buffer");
    check(sample->SetSampleTime(pts),"Sample timestamp");check(sample->SetSampleDuration(duration),"Sample duration");return sample;
}
ComPtr<IMFSample> surfaceSample(ID3D11Texture2D* texture,LONGLONG pts,LONGLONG duration) {
    ComPtr<IMFMediaBuffer> buffer;check(MFCreateDXGISurfaceBuffer(__uuidof(ID3D11Texture2D),texture,0,FALSE,&buffer),"DXGI sample buffer");
    ComPtr<IMF2DBuffer> b2d;check(buffer.As(&b2d),"DXGI 2D buffer");DWORD length=0;
    check(b2d->GetContiguousLength(&length),"DXGI buffer size");check(buffer->SetCurrentLength(length),"DXGI buffer length");
    ComPtr<IMFSample> sample;check(MFCreateSample(&sample),"DXGI sample");check(sample->AddBuffer(buffer.Get()),"DXGI sample add buffer");
    check(sample->SetSampleTime(pts),"DXGI sample timestamp");check(sample->SetSampleDuration(duration),"DXGI sample duration");return sample;
}
bool codecValue(ICodecAPI* codec,const GUID& key,ULONG value,bool boolean=false,bool required=false) {
    VARIANT v;VariantInit(&v);v.vt=boolean?VT_BOOL:VT_UI4;
    if(boolean)v.boolVal=value?VARIANT_TRUE:VARIANT_FALSE;else v.ulVal=value;
    HRESULT hr=codec->SetValue(&key,&v);
    if(required)check(hr,"Required codec property","encoder_config_failed");return SUCCEEDED(hr);
}

// All MFT methods are invoked on the media thread. Async credits come exclusively from MF events.
class Transform {
    State& state_;
    ComPtr<IMFMediaEventGenerator> events_;
    bool async_=false,started_=false,drained_=false;
    unsigned inputCredits_=0;
    bool outputOnce() {
        for(unsigned attempt=0;attempt<8;++attempt) {
            MFT_OUTPUT_STREAM_INFO info{};check(mft->GetOutputStreamInfo(outputID,&info),"MFT output info");
            ComPtr<IMFSample> provided;
            if(!(info.dwFlags&(MFT_OUTPUT_STREAM_PROVIDES_SAMPLES|MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))) {
                if(!info.cbSize||info.cbSize>wire::MaxPayload)throw Error("frame_too_large","MFT output allocation is out of bounds",6);
                check(MFCreateSample(&provided),"MFT output sample");ComPtr<IMFMediaBuffer> buffer;
                check(MFCreateAlignedMemoryBuffer(info.cbSize,info.cbAlignment?info.cbAlignment-1:0,&buffer),"MFT output buffer");
                check(provided->AddBuffer(buffer.Get()),"MFT output add buffer");
            }
            MFT_OUTPUT_DATA_BUFFER output{};output.dwStreamID=outputID;output.pSample=provided.Get();DWORD status=0;
            HRESULT hr=mft->ProcessOutput(0,1,&output,&status);
            ComPtr<IMFCollection> outputEvents;outputEvents.Attach(output.pEvents);
            ComPtr<IMFSample> sample;
            if(output.pSample==provided.Get())sample=provided;else sample.Attach(output.pSample);
            if(hr==MF_E_TRANSFORM_STREAM_CHANGE) {
                onFormat();
                // One asynchronous HaveOutput event permits one ProcessOutput
                // call, including a stream-change response. A new event owns
                // the next call; synchronous transforms may retry immediately.
                if(async_)return true;
                continue;
            }
            if(hr==MF_E_TRANSFORM_NEED_MORE_INPUT) {
                if(async_)throw Error("mft_state","Async MFT requested input from ProcessOutput");return false;
            }
            check(hr,"MFT ProcessOutput","mft_output_failed");
            if(sample && (output.dwStatus&MFT_OUTPUT_DATA_BUFFER_NO_SAMPLE)!=MFT_OUTPUT_DATA_BUFFER_NO_SAMPLE)onOutput(sample.Get());
            return true;
        }
        throw Error("mft_stream_change","MFT repeatedly changed its output type");
    }
public:
    ComPtr<IMFTransform> mft;
    DWORD inputID=0,outputID=0;
    std::function<void(IMFSample*)> onOutput;
    std::function<void()> onFormat;
    Transform(State& state,ComPtr<IMFTransform> transform,Gpu& gpu,bool requireAsync)
        :state_(state),mft(transform) {
        ComPtr<IMFAttributes> a;check(mft->GetAttributes(&a),"MFT attributes");
        UINT32 v=0;if(SUCCEEDED(a->GetUINT32(MF_TRANSFORM_ASYNC,&v)))async_=v!=0;
        if(requireAsync&&!async_)throw Error("hw_encoder_unavailable","Hardware MFT did not report asynchronous operation",2);
        if(async_) {check(a->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK,TRUE),"Unlock asynchronous MFT");check(mft.As(&events_),"MFT event generator");}
        v=0;check(a->GetUINT32(MF_SA_D3D11_AWARE,&v),"D3D11-aware attribute");
        if(!v)throw Error("hw_codec_unavailable","MFT does not support D3D11 surfaces",2);
        check(mft->ProcessMessage(MFT_MESSAGE_SET_D3D_MANAGER,reinterpret_cast<ULONG_PTR>(gpu.manager.Get())),"MFT D3D11 manager","hw_codec_unavailable",2);
        DWORD inputs=0,outputs=0;check(mft->GetStreamCount(&inputs,&outputs),"MFT stream count");
        if(inputs!=1||outputs!=1)throw Error("mft_streams","Exactly one MFT video stream is required");
        HRESULT hr=mft->GetStreamIDs(1,&inputID,1,&outputID);
        if(hr!=E_NOTIMPL)check(hr,"MFT stream IDs");
    }
    ~Transform() {
        CleanupWatch cleanup(state_);
        if(mft) {
            if(started_) {mft->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH,0);mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING,0);}
            mft->ProcessMessage(MFT_MESSAGE_SET_D3D_MANAGER,0);
            ComPtr<IMFShutdown> shutdown;if(SUCCEEDED(mft.As(&shutdown)))shutdown->Shutdown();
        }
    }
    void start() {
        check(mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING,0),"MFT begin streaming");
        check(mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM,0),"MFT start stream");started_=true;
    }
    void pump() {
        if(!started_)return;
        if(!async_) {for(unsigned i=0;i<128&&outputOnce();++i){}return;}
        for(unsigned i=0;i<256;++i) {
            ComPtr<IMFMediaEvent> event;HRESULT hr=events_->GetEvent(MF_EVENT_FLAG_NO_WAIT,&event);
            if(hr==MF_E_NO_EVENTS_AVAILABLE)break;check(hr,"MFT GetEvent");
            HRESULT status=S_OK;check(event->GetStatus(&status),"MFT event status");check(status,"MFT asynchronous operation","mft_async_failed");
            MediaEventType type=MEUnknown;check(event->GetType(&type),"MFT event type");
            if(type==METransformNeedInput) {if(++inputCredits_>64)throw Error("mft_state","Too many MFT input credits");}
            else if(type==METransformHaveOutput) outputOnce();
            else if(type==METransformDrainComplete)drained_=true;
            else if(type==MEError)throw Error("mft_async_failed","MFT reported an error event");
        }
    }
    bool input(IMFSample* sample) {
        const auto deadline=Clock::now()+5s;
        for(;;) {
            if(state_.stop)return false;
            pump();
            if(!async_||inputCredits_) {
                HRESULT hr=mft->ProcessInput(inputID,sample,0);
                if(SUCCEEDED(hr)) {if(async_)--inputCredits_;return true;}
                if(hr!=MF_E_NOTACCEPTING)check(hr,"MFT ProcessInput","mft_input_failed");
                if(async_)throw Error("mft_state","Async MFT rejected an advertised input credit");
            }
            if(Clock::now()>deadline)throw Error("mft_timeout","MFT input did not progress");
            Sleep(1);
        }
    }
    void drain() {
        if(!started_)return;
        check(mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM,inputID),"MFT end stream");
        check(mft->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN,0),"MFT drain");
        const auto deadline=Clock::now()+1800ms;
        do {
            pump();
            if(!async_||drained_)break;
            if(Clock::now()>deadline)throw Error("mft_timeout","MFT drain did not finish");
            Sleep(1);
        }while(true);
        check(mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING,0),"MFT end streaming");started_=false;
    }
};
std::vector<ComPtr<IMFActivate>> hardwareEncoders(Gpu& gpu) {
    MFT_REGISTER_TYPE_INFO input{MFMediaType_Video,MFVideoFormat_NV12},output{MFMediaType_Video,MFVideoFormat_H264};
    IMFActivate** raw=nullptr;UINT32 count=0;
    using Enum2=HRESULT(WINAPI*)(GUID,UINT32,const MFT_REGISTER_TYPE_INFO*,const MFT_REGISTER_TYPE_INFO*,IMFAttributes*,IMFActivate***,UINT32*);
    auto enum2=reinterpret_cast<Enum2>(GetProcAddress(GetModuleHandleW(L"mfplat.dll"),"MFTEnum2"));
    HRESULT hr;
    if(enum2) {
        ComPtr<IMFAttributes> a;check(MFCreateAttributes(&a,1),"Hardware enumeration attributes");
        check(a->SetBlob(MFT_ENUM_ADAPTER_LUID,reinterpret_cast<const UINT8*>(&gpu.desc.AdapterLuid),sizeof(LUID)),"Hardware enumeration LUID");
        hr=enum2(MFT_CATEGORY_VIDEO_ENCODER,MFT_ENUM_FLAG_HARDWARE|MFT_ENUM_FLAG_SORTANDFILTER,&input,&output,a.Get(),&raw,&count);
    } else hr=MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,MFT_ENUM_FLAG_HARDWARE|MFT_ENUM_FLAG_SORTANDFILTER,&input,&output,&raw,&count);
    struct ActivationArray {
        IMFActivate** items;UINT32 count;
        ~ActivationArray(){if(items){for(UINT32 i=0;i<count;++i)if(items[i])items[i]->Release();CoTaskMemFree(items);}}
    } owned{raw,count};
    check(hr,"Hardware encoder enumeration","hw_encoder_unavailable",2);
    std::vector<ComPtr<IMFActivate>> list;list.reserve(count);
    for(UINT32 i=0;i<count;++i){ComPtr<IMFActivate> a;a.Attach(raw[i]);raw[i]=nullptr;list.push_back(std::move(a));}
    return list;
}
struct Encoded { uint64_t unixUs=0; bool idr=false; Bytes data; };
class Encoder {
    State& state_;
    Gpu& gpu_;
    Options options_;
    ComPtr<IMFActivate> activation_;
    ComPtr<ICodecAPI> codec_;
    std::unique_ptr<Transform> transform_;
    Converter converter_;
    std::map<LONGLONG,uint64_t> timestamps_;
    LONGLONG serial_=0,lastIDR_=0;
    unsigned lengthSize_=4;
    void confirmControls() {
        VARIANT b;VariantInit(&b);HRESULT hr=codec_->GetValue(&CODECAPI_AVEncMPVDefaultBPictureCount,&b);
        const bool zero=SUCCEEDED(hr)&&((b.vt==VT_UI4&&b.ulVal==0)||(b.vt==VT_I4&&b.lVal==0));VariantClear(&b);
        if(!zero)throw Error("encoder_config_failed","Encoder did not confirm zero B frames");
        if(lowLatency) {
            VARIANT value;VariantInit(&value);HRESULT result=codec_->GetValue(&CODECAPI_AVLowLatencyMode,&value);
            lowLatency=SUCCEEDED(result)&&((value.vt==VT_BOOL&&value.boolVal!=VARIANT_FALSE)||(value.vt==VT_UI4&&value.ulVal!=0));
            VariantClear(&value);
        }
    }
    void sequence() {
        ComPtr<IMFMediaType> t;check(transform_->mft->GetOutputCurrentType(transform_->outputID,&t),"Encoder output type");
        GUID subtype{};check(t->GetGUID(MF_MT_SUBTYPE,&subtype),"Encoder subtype");
        if(subtype!=MFVideoFormat_H264)throw Error("encoder_format","Encoder changed away from H264");
        UINT32 size=0;
        if(SUCCEEDED(t->GetBlobSize(MF_MT_MPEG_SEQUENCE_HEADER,&size))&&size) {
            if(size>1u<<20)throw Error("encoder_format","Encoder sequence header is too large");
            Bytes header(size);check(t->GetBlob(MF_MT_MPEG_SEQUENCE_HEADER,header.data(),size,nullptr),"Encoder sequence header");
            parameters.update(wire::sequenceHeader(header,lengthSize_));
        }
    }
    void output(IMFSample* sample) {
        ComPtr<IMFMediaBuffer> buffer;check(sample->ConvertToContiguousBuffer(&buffer),"Encoded contiguous buffer");
        BYTE* p=nullptr;DWORD n=0;check(buffer->Lock(&p,nullptr,&n),"Encoded buffer lock");
        if(n>wire::MaxPayload-9) {buffer->Unlock();throw Error("payload_too_large","Encoded access unit exceeds limit",6);}
        if(!n){buffer->Unlock();return;}
        Bytes data;
        try{data.assign(p,p+n);}catch(...){buffer->Unlock();throw;}
        check(buffer->Unlock(),"Encoded buffer unlock");
        auto au=parameters.au(wire::encodedNals(data,lengthSize_));
        if(!au.slices)return; // Parameter-only / EOS output need not carry an input timestamp.
        LONGLONG pts=0;check(sample->GetSampleTime(&pts),"Encoder output timestamp");
        auto it=timestamps_.find(pts);
        if(it==timestamps_.end())throw Error("encoder_timestamp","Encoder returned an unmapped timestamp");
        const uint64_t us=it->second;
        const auto size=wire::sps(parameters.spsBytes);
        if(size.width!=options_.width||size.height!=options_.height)throw Error("encoder_format","Encoder SPS changed the requested dimensions");
        timestamps_.erase(it);
        if(firstOutput&&!au.idr)throw Error("encoder_format","First encoded access unit was not an IDR");
        firstOutput=false;verified=true;++outputCount;
        onEncoded({us,au.idr,std::move(au.annexB)});
    }
public:
    wire::ParameterSets parameters;
    std::function<void(Encoded)> onEncoded;
    std::string encoderID;
    bool verified=false,lowLatency=false,firstOutput=true;
    UINT currentBitrate()const{return options_.bitrate;}
    uint64_t outputCount=0;
    Encoder(State& state,Gpu& gpu,const Options& o,ComPtr<IMFActivate> activation)
        :state_(state),gpu_(gpu),options_(o),activation_(activation),converter_(gpu) {
        UINT32 urlLength=0;check(activation_->GetStringLength(MFT_ENUM_HARDWARE_URL_Attribute,&urlLength),"Hardware MFT registration","hw_encoder_unavailable",2);
        if(!urlLength)throw Error("hw_encoder_unavailable","Hardware MFT registration lacks a hardware URL",2);
        GUID clsid{};check(activation_->GetGUID(MFT_TRANSFORM_CLSID_Attribute,&clsid),"Hardware MFT CLSID");encoderID=guidString(clsid);
        ComPtr<IMFTransform> mft;check(activation_->ActivateObject(IID_PPV_ARGS(&mft)),"Hardware encoder activation","hw_encoder_unavailable",2);
        transform_=std::make_unique<Transform>(state,mft,gpu,true);
        check(mft.As(&codec_),"Encoder codec controls","encoder_config_failed");
        codecValue(codec_.Get(),CODECAPI_AVEncMPVDefaultBPictureCount,0,false,true);
        codecValue(codec_.Get(),CODECAPI_AVEncCommonRateControlMode,eAVEncCommonRateControlMode_CBR,false,true);
        if(o.rate=="lowlatency") {
            if(codecValue(codec_.Get(),CODECAPI_AVLowLatencyMode,1,true)) {
                VARIANT value;VariantInit(&value);
                HRESULT hr=codec_->GetValue(&CODECAPI_AVLowLatencyMode,&value);
                lowLatency=SUCCEEDED(hr)&&((value.vt==VT_BOOL&&value.boolVal!=VARIANT_FALSE)||(value.vt==VT_UI4&&value.ulVal!=0));
                VariantClear(&value);
            }
            if(!lowLatency)log("optional_low_latency_not_confirmed_using_hardware_cbr_no_b");
        } else codecValue(codec_.Get(),CODECAPI_AVLowLatencyMode,0,true);
        codecValue(codec_.Get(),CODECAPI_AVEncCommonRealTime,1,true);
        codecValue(codec_.Get(),CODECAPI_AVEncMPVGOPSize,ULONG(std::max(1.0,o.keyInterval*o.fps)),false,true);
        auto out=videoType(MFVideoFormat_H264,o.width,o.height,o.fps);
        check(out->SetUINT32(MF_MT_AVG_BITRATE,o.bitrate),"Encoder output bitrate");
        check(out->SetUINT32(MF_MT_MPEG2_PROFILE,eAVEncH264VProfile_High),"Encoder High profile");
        check(mft->SetOutputType(transform_->outputID,out.Get(),0),"Encoder H264 output type","encoder_config_failed");
        auto in=videoType(MFVideoFormat_NV12,o.width,o.height,o.fps);
        check(in->SetUINT32(MF_MT_DEFAULT_STRIDE,o.width),"Encoder NV12 stride");
        check(in->SetUINT32(MF_MT_SAMPLE_SIZE,o.width*o.height*3/2),"Encoder NV12 sample size");
        check(mft->SetInputType(transform_->inputID,in.Get(),0),"Encoder NV12 input type","encoder_config_failed");
        setBitrate(o.bitrate);
        confirmControls();
        transform_->onOutput=[this](IMFSample* s){output(s);};
        transform_->onFormat=[this]{
            ComPtr<IMFMediaType> t;
            for(DWORD i=0;;++i) {
                HRESULT hr=transform_->mft->GetOutputAvailableType(transform_->outputID,i,&t);
                if(hr==MF_E_NO_MORE_TYPES)throw Error("encoder_format","No H264 output type after stream change");
                check(hr,"Encoder stream-change type");GUID subtype{};t->GetGUID(MF_MT_SUBTYPE,&subtype);
                if(subtype==MFVideoFormat_H264) {
                    // Available types may be partial. Complete geometry, rate, aspect and progressive/color defaults.
                    auto fixed=videoType(MFVideoFormat_H264,options_.width,options_.height,options_.fps);
                    check(fixed->CopyAllItems(t.Get()),"Encoder stream-change defaults");
                    check(t->SetUINT32(MF_MT_AVG_BITRATE,options_.bitrate),"Encoder stream-change bitrate");
                    check(t->SetUINT32(MF_MT_MPEG2_PROFILE,eAVEncH264VProfile_High),"Encoder stream-change profile");
                    codecValue(codec_.Get(),CODECAPI_AVEncMPVDefaultBPictureCount,0);
                    check(transform_->mft->SetOutputType(transform_->outputID,t.Get(),0),"Encoder stream-change negotiation");
                    setBitrate(options_.bitrate);confirmControls();sequence();return;
                }
                t.Reset();
            }
        };
        sequence();transform_->start();
    }
    ~Encoder() {CleanupWatch cleanup(state_);transform_.reset();if(activation_)activation_->ShutdownObject();}
    void setBitrate(UINT bitrate) {
        codecValue(codec_.Get(),CODECAPI_AVEncCommonMeanBitRate,bitrate,false,true);options_.bitrate=bitrate;
    }
    void pump(){transform_->pump();}
    bool encode(ID3D11Texture2D* bgra,uint64_t us,D3D11_VIDEO_PROCESSOR_ROTATION rotation=D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY) {
        const auto deadline=Clock::now()+5s;
        // Hardware pipelines may request another input before producing their
        // first output, even without B frames. Honor those event credits with
        // a finite budget rather than deadlocking a valid three-frame MFT.
        constexpr size_t maxInFlight=32;
        while(timestamps_.size()>=maxInFlight&&!state_.stop) {
            pump();if(Clock::now()>deadline)throw Error("mft_timeout","Encoder in-flight frames did not finish");Sleep(1);
        }
        if(state_.stop)return false;
        if(auto bitrate=state_.newBitrate.exchange(0))setBitrate(bitrate);
        const LONGLONG pts=++serial_*10000000/options_.fps;
        const bool force=state_.forceIDR.exchange(false)||lastIDR_==0||pts-lastIDR_>=LONGLONG(options_.keyInterval*10000000);
        if(force){codecValue(codec_.Get(),CODECAPI_AVEncVideoForceKeyFrame,1,false,true);lastIDR_=pts;}
        D3D11_TEXTURE2D_DESC d{};bgra->GetDesc(&d);RECT rect{0,0,LONG(d.Width),LONG(d.Height)};
        auto nv12=converter_.process(bgra,0,options_.width,options_.height,DXGI_FORMAT_NV12,rect,rotation);
        auto sample=surfaceSample(nv12.Get(),pts,10000000/options_.fps);
        timestamps_.emplace(pts,us);
        try {if(!transform_->input(sample.Get())){timestamps_.erase(pts);return false;}}
        catch(...){timestamps_.erase(pts);throw;}
        pump();return true;
    }
    void finish(){transform_->drain();if(!timestamps_.empty())throw Error("encoder_incomplete","Encoder discarded input during drain");}
    std::string evidence() const {
        if(!verified)throw Error("hw_encoder_unavailable","Encoder has not produced a verified access unit",2);
        return "MFT_ENUM_FLAG_HARDWARE; hardware URL present; async D3D11-aware MFT; device manager accepted; actual GPU NV12 input -> H264 SPS/IDR; BPictureCount readback=0; all slices checked; adapter="+gpu_.luid();
    }
};
std::unique_ptr<Encoder> makeEncoder(State& state,Gpu& gpu,const Options& o) {
    auto list=hardwareEncoders(gpu);
    for(auto& a:list) {
        try {return std::make_unique<Encoder>(state,gpu,o,a);}
        catch(const Error& e){a->ShutdownObject();log("encoder_candidate_rejected code="+e.code);}
    }
    throw Error("hw_encoder_unavailable","No hardware H264 MFT accepted the required NV12/low-latency/no-B configuration",2);
}

class Decoder {
    State& state_;
    Gpu& gpu_;
    std::unique_ptr<Transform> transform_;
    Converter converter_;
    wire::ParameterSets parameters_;
    wire::Dimensions dimensions_{};
    std::map<LONGLONG,uint64_t> timestamps_;
    LONGLONG serial_=0;
    bool needIDR_=true;
    Clock::time_point lastEvent_{};
    void negotiate(bool afterStreamChange) {
        for(DWORD i=0;;++i) {
            ComPtr<IMFMediaType> t;HRESULT hr=transform_->mft->GetOutputAvailableType(transform_->outputID,i,&t);
            if(hr==MF_E_NO_MORE_TYPES)throw Error("hw_decoder_unavailable","Decoder offers no NV12 hardware output",2);
            check(hr,"Decoder output types");GUID subtype{};t->GetGUID(MF_MT_SUBTYPE,&subtype);
            if(subtype==MFVideoFormat_NV12) {
                check(transform_->mft->SetOutputType(transform_->outputID,t.Get(),0),"Decoder NV12 output type","hw_decoder_unavailable",2);
                MFT_OUTPUT_STREAM_INFO info{};check(transform_->mft->GetOutputStreamInfo(transform_->outputID,&info),"Decoder hardware stream info");
                // Before SPS ingestion this can still be a placeholder type. After the real format
                // change, hardware decoding must allocate its own surfaces. Every output is also checked.
                if(afterStreamChange&&!(info.dwFlags&MFT_OUTPUT_STREAM_PROVIDES_SAMPLES))
                    throw Error("hw_decoder_unavailable","Decoder requires CPU output buffers; hardware fallback is forbidden",2);
                return;
            }
        }
    }
    void output(IMFSample* sample) {
        LONGLONG pts=0;check(sample->GetSampleTime(&pts),"Decoder output timestamp");
        auto it=timestamps_.find(pts);if(it==timestamps_.end())throw Error("decoder_timestamp","Decoder returned an unmapped timestamp");
        ComPtr<IMFMediaBuffer> buffer;check(sample->GetBufferByIndex(0,&buffer),"Decoder output buffer");
        ComPtr<IMFDXGIBuffer> dxgi;
        check(buffer.As(&dxgi),"Decoder GPU buffer","hw_decoder_unavailable",2);
        ComPtr<ID3D11Texture2D> surface;check(dxgi->GetResource(IID_PPV_ARGS(&surface)),"Decoder GPU texture","hw_decoder_unavailable",2);
        UINT subresource=0;check(dxgi->GetSubresourceIndex(&subresource),"Decoder texture subresource");
        ComPtr<ID3D11Device> device;surface->GetDevice(&device);
        ComPtr<IUnknown> actual,expected;check(device.As(&actual),"Decoder device identity");check(gpu_.device.As(&expected),"GPU device identity");
        D3D11_TEXTURE2D_DESC d{};surface->GetDesc(&d);
        if(actual.Get()!=expected.Get()||d.Format!=DXGI_FORMAT_NV12||!(d.BindFlags&D3D11_BIND_DECODER)||
           d.Usage!=D3D11_USAGE_DEFAULT||d.CPUAccessFlags||subresource>=d.MipLevels*d.ArraySize)
            throw Error("hw_decoder_unavailable","Decoder did not return a hardware NV12 decoder-bound surface on the supplied device",2);
        RECT crop{LONG(dimensions_.left),LONG(dimensions_.top),LONG(dimensions_.left+dimensions_.width),LONG(dimensions_.top+dimensions_.height)};
        auto bgra=converter_.process(surface.Get(),subresource,dimensions_.width,dimensions_.height,DXGI_FORMAT_B8G8R8A8_UNORM,crop);
        Bytes pixels=readBGRA(gpu_,bgra.Get());
        for(size_t j=0;j<pixels.size();j+=4){std::swap(pixels[j],pixels[j+2]);pixels[j+3]=255;}
        const uint64_t us=it->second;timestamps_.erase(it);verified=true;++outputCount;
        onFrame(us,dimensions_.width,dimensions_.height,std::move(pixels));
    }
    void open() {
        dimensions_=wire::sps(parameters_.spsBytes);
        ComPtr<IMFTransform> mft;
        check(CoCreateInstance(CLSID_CMSH264DecoderMFT,nullptr,CLSCTX_INPROC_SERVER,IID_PPV_ARGS(&mft)),"Microsoft H264 decoder","hw_decoder_unavailable",2);
        transform_=std::make_unique<Transform>(state_,mft,gpu_,false);
        ComPtr<ICodecAPI> codec;if(SUCCEEDED(mft.As(&codec)))codecValue(codec.Get(),CODECAPI_AVLowLatencyMode,1,true);
        ComPtr<IMFAttributes> a;check(mft->GetAttributes(&a),"Decoder attributes");
        // Acceleration_H264 is intentionally not used: Microsoft documents that it has no effect for MF.
        check(a->SetUINT32(MF_LOW_LATENCY,TRUE),"Decoder low latency");
        auto input=videoType(MFVideoFormat_H264,dimensions_.width,dimensions_.height,30);
        check(mft->SetInputType(transform_->inputID,input.Get(),0),"Decoder H264 input type","hw_decoder_unavailable",2);
        transform_->onOutput=[this](IMFSample* s){output(s);};transform_->onFormat=[this]{negotiate(true);};
        negotiate(false);transform_->start();needIDR_=false;
    }
    void request(const char* reason) {
        const auto now=Clock::now();if(now-lastEvent_<200ms)return;lastEvent_=now;if(onNeedIDR)onNeedIDR(reason);
    }
    void recover(const Error& e) {
        if(e.code!="mft_input_failed"&&e.code!="mft_output_failed"&&e.code!="mft_async_failed")throw e;
        transform_.reset();timestamps_.clear();needIDR_=true;
        log("decoder_recovery code="+e.code);request("decode_error");
    }
public:
    bool verified=false;
    uint64_t outputCount=0;
    std::function<void(uint64_t,UINT,UINT,Bytes)> onFrame;
    std::function<void(const char*)> onNeedIDR;
    Decoder(State& state,Gpu& gpu):state_(state),gpu_(gpu),converter_(gpu){}
    void pump(){try{if(transform_)transform_->pump();}catch(const Error& e){recover(e);}}
    void decode(const Bytes& data,uint64_t us) {
        auto au=parameters_.au(wire::annex(data));
        if(au.parameterChange && transform_) {
            // Drain using OLD crop/format, then rebuild only at an IDR.
            transform_->drain();
            if(!timestamps_.empty())throw Error("decoder_incomplete","Decoder discarded input at a parameter-set change");
            transform_.reset();needIDR_=true;
        }
        if(!au.slices)return;
        if(needIDR_&&!au.idr){request("waiting_for_idr");return;}
        if(!transform_)open();
        if(timestamps_.size()>=32)throw Error("decoder_backpressure","Decoder retained too many input frames");
        const LONGLONG pts=++serial_*333333;
        auto sample=byteSample(au.annexB,pts,333333);
        if(au.idr)check(sample->SetUINT32(MFSampleExtension_CleanPoint,TRUE),"Decoder clean point");
        timestamps_.emplace(pts,us);
        try {if(!transform_->input(sample.Get())){timestamps_.erase(pts);return;}}
        catch(const Error& e){timestamps_.erase(pts);recover(e);return;}
        pump();
    }
    void finish() {
        if(transform_)transform_->drain();
        if(!timestamps_.empty())throw Error("decoder_incomplete","Decoder discarded input during drain");
    }
    std::string evidence() const {
        if(!verified)throw Error("hw_decoder_unavailable","Decoder has not returned a verified GPU picture",2);
        return "CLSID_CMSH264DecoderMFT; MF_SA_D3D11_AWARE; hardware D3D11 manager accepted; actual NV12 IMFDXGIBuffer with D3D11_BIND_DECODER on supplied hardware device; GPU video conversion and pitched readback verified; adapter="+gpu_.luid();
    }
};
// Ordinary interactive Default desktop only. Never switches desktops or elevates.
std::string desktopRestriction() {
    DWORD session=0;if(!ProcessIdToSessionId(GetCurrentProcessId(),&session)||!session)return "noninteractive_session";
    LPWSTR raw=nullptr;DWORD bytes=0;
    if(!WTSQuerySessionInformationW(WTS_CURRENT_SERVER_HANDLE,session,WTSConnectState,&raw,&bytes))return "session_state_unavailable";
    const bool active=bytes>=sizeof(WTS_CONNECTSTATE_CLASS)&&*reinterpret_cast<WTS_CONNECTSTATE_CLASS*>(raw)==WTSActive;
    WTSFreeMemory(raw);raw=nullptr;if(!active)return "session_disconnected";
    if(!WTSQuerySessionInformationW(WTS_CURRENT_SERVER_HANDLE,session,WTSSessionInfoEx,&raw,&bytes))return "session_lock_state_unavailable";
    bool unlocked=false;
    if(bytes>=sizeof(WTSINFOEXW)) {const auto* info=reinterpret_cast<const WTSINFOEXW*>(raw);unlocked=info->Level==1&&info->Data.WTSInfoExLevel1.SessionFlags==WTS_SESSIONSTATE_UNLOCK;}
    WTSFreeMemory(raw);if(!unlocked)return "session_locked";
    HDESK input=OpenInputDesktop(0,FALSE,DESKTOP_READOBJECTS);
    if(!input)return "secure_or_inaccessible_desktop";
    wchar_t name[256]{},own[256]{};DWORD used=0;
    BOOL a=GetUserObjectInformationW(input,UOI_NAME,name,sizeof(name),&used);CloseDesktop(input);
    BOOL b=GetUserObjectInformationW(GetThreadDesktop(GetCurrentThreadId()),UOI_NAME,own,sizeof(own),&used);
    if(!a||!b||_wcsicmp(name,L"Default")||_wcsicmp(name,own))return "secure_or_other_desktop";
    return {};
}
void requireDesktop() {
    auto why=desktopRestriction();if(!why.empty())throw Error("capture_unavailable",why,4);
}
D3D11_VIDEO_PROCESSOR_ROTATION rotation(DXGI_MODE_ROTATION r) {
    switch(r) {
        case DXGI_MODE_ROTATION_IDENTITY:case DXGI_MODE_ROTATION_UNSPECIFIED:return D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY;
        case DXGI_MODE_ROTATION_ROTATE90:return D3D11_VIDEO_PROCESSOR_ROTATION_90;
        case DXGI_MODE_ROTATION_ROTATE180:return D3D11_VIDEO_PROCESSOR_ROTATION_180;
        case DXGI_MODE_ROTATION_ROTATE270:return D3D11_VIDEO_PROCESSOR_ROTATION_270;
        default:throw Error("capture_unavailable","Unknown display rotation",4);
    }
}
class Capture {
    State& state_;
    Gpu& gpu_;
    Display display_;
    UINT outputWidth_,outputHeight_;
    Clock::time_point waitingSince_=Clock::now();
    Converter upright_;
    ComPtr<IDXGIOutputDuplication> duplication_;
    ComPtr<ID3D11Texture2D> image_;
    DXGI_OUTDUPL_POINTER_POSITION pointer_{};
    DXGI_OUTDUPL_POINTER_SHAPE_INFO shape_{};
    Bytes pointerBytes_;
    void create() {
        requireDesktop();DXGI_OUTPUT_DESC current{};check(display_.output->GetDesc(&current),"Capture display description","capture_unavailable",4);
        if(!current.AttachedToDesktop||std::memcmp(&current.DesktopCoordinates,&display_.desc.DesktopCoordinates,sizeof(RECT))||current.Rotation!=display_.desc.Rotation)
            throw Error("capture_geometry_changed","Selected display geometry changed; host must restart capture",4);
        check(display_.output->DuplicateOutput(gpu_.device.Get(),&duplication_),"Desktop duplication","capture_unavailable",4);
        waitingSince_=Clock::now();
    }
    void recover() {
        duplication_.Reset();image_.Reset();pointerBytes_.clear();pointer_={};
        for(unsigned i=0;i<3&&!state_.stop;++i) {
            requireDesktop();
            try {create();state_.forceIDR=true;return;}
            catch(const Error& e) {if(e.code=="capture_geometry_changed")throw;if(i==2)throw;Sleep(50);}
        }
    }
public:
    uint64_t timestamp=0;
    Capture(State& s,Gpu& gpu,const Display& d,UINT width,UINT height)
        :state_(s),gpu_(gpu),display_(d),outputWidth_(width),outputHeight_(height),upright_(gpu){create();}
    bool hasImage()const{return image_!=nullptr;}
    bool initialFrameTimedOut()const{return !image_&&Clock::now()-waitingSince_>5s;}
    bool acquire(const Timebase& time) {
        requireDesktop();DXGI_OUTDUPL_FRAME_INFO info{};ComPtr<IDXGIResource> resource;
        HRESULT hr=duplication_->AcquireNextFrame(20,&info,&resource);
        if(hr==DXGI_ERROR_WAIT_TIMEOUT)return false;
        if(hr==DXGI_ERROR_ACCESS_LOST){recover();return false;}
        check(hr,"Acquire desktop frame","capture_unavailable",4);
        struct Release {IDXGIOutputDuplication* p;~Release(){p->ReleaseFrame();}} release{duplication_.Get()};
        if(info.ProtectedContentMaskedOut)throw Error("capture_protected_content","Desktop duplication reported protected content",4);
        ComPtr<ID3D11Texture2D> source;check(resource.As(&source),"Desktop frame texture","capture_unavailable",4);
        D3D11_TEXTURE2D_DESC d{};source->GetDesc(&d);
        const bool swap=display_.desc.Rotation==DXGI_MODE_ROTATION_ROTATE90||display_.desc.Rotation==DXGI_MODE_ROTATION_ROTATE270;
        if(d.Format!=DXGI_FORMAT_B8G8R8A8_UNORM||(swap?d.Height:d.Width)!=display_.width||(swap?d.Width:d.Height)!=display_.height)
            throw Error("capture_geometry_changed","Desktop duplication texture geometry changed",4);
        if(!image_)image_=gpu_.texture(d.Width,d.Height,d.Format);
        gpu_.context->CopyResource(image_.Get(),source.Get());
        if(info.LastMouseUpdateTime.QuadPart)pointer_=info.PointerPosition;
        if(info.PointerShapeBufferSize) {
            if(info.PointerShapeBufferSize>1024u*1024u)throw Error("capture_unavailable","Cursor shape exceeds 1 MiB",4);
            pointerBytes_.resize(info.PointerShapeBufferSize);UINT used=0;
            check(duplication_->GetFramePointerShape(UINT(pointerBytes_.size()),pointerBytes_.data(),&used,&shape_),"Cursor shape","capture_unavailable",4);
            if(used>pointerBytes_.size())throw Error("capture_unavailable","Cursor shape changed during copy",4);
            pointerBytes_.resize(used);
            if(!shape_.Width||!shape_.Height||shape_.Width>4096||shape_.Height>8192||uint64_t(shape_.Pitch)*shape_.Height>pointerBytes_.size())
                throw Error("capture_unavailable","Invalid cursor dimensions",4);
            if(shape_.Type==DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME) {
                if((shape_.Height&1)||shape_.Pitch<(shape_.Width+7)/8)throw Error("capture_unavailable","Invalid monochrome cursor stride",4);
            } else if((shape_.Type!=DXGI_OUTDUPL_POINTER_SHAPE_TYPE_COLOR&&shape_.Type!=DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR)||uint64_t(shape_.Width)*4>shape_.Pitch)
                throw Error("capture_unavailable","Unsupported cursor shape",4);
        }
        const int64_t pts=std::max(info.LastPresentTime.QuadPart,info.LastMouseUpdateTime.QuadPart);
        timestamp=time.micros(pts>0?pts:time.now());return true;
    }
    ComPtr<ID3D11Texture2D> picture(bool cursor) {
        if(!image_)return nullptr;
        ComPtr<ID3D11Texture2D> image=image_;
        const auto r=rotation(display_.desc.Rotation);
        D3D11_TEXTURE2D_DESC d{};image_->GetDesc(&d);
        if(r!=D3D11_VIDEO_PROCESSOR_ROTATION_IDENTITY||d.Width!=outputWidth_||d.Height!=outputHeight_) {
            RECT rect{0,0,LONG(d.Width),LONG(d.Height)};
            image=upright_.process(image_.Get(),0,outputWidth_,outputHeight_,DXGI_FORMAT_B8G8R8A8_UNORM,rect,r);
        }
        if(!cursor||!pointer_.Visible)return image;
        if(pointerBytes_.empty())throw Error("capture_unavailable","Visible hardware cursor has no shape",4);
        // Cursor coordinates are in upright output space. HotSpot is informational only (DXGI contract).
        Bytes pixels=readBGRA(gpu_,image.Get());
        const UINT height=shape_.Type==DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME?shape_.Height/2:shape_.Height;
        const int64_t left=std::max<int64_t>(0,int64_t(pointer_.Position.x)*outputWidth_/display_.width);
        const int64_t top=std::max<int64_t>(0,int64_t(pointer_.Position.y)*outputHeight_/display_.height);
        const int64_t right=std::min<int64_t>(outputWidth_,((int64_t(pointer_.Position.x)+shape_.Width)*outputWidth_+display_.width-1)/display_.width);
        const int64_t bottom=std::min<int64_t>(outputHeight_,((int64_t(pointer_.Position.y)+height)*outputHeight_+display_.height-1)/display_.height);
        for(int64_t py=top;py<bottom;++py)for(int64_t px=left;px<right;++px) {
            const int64_t sx=(2*px+1)*display_.width/(2*outputWidth_)-pointer_.Position.x;
            const int64_t sy=(2*py+1)*display_.height/(2*outputHeight_)-pointer_.Position.y;
            if(sx<0||sy<0||sx>=shape_.Width||sy>=height)continue;
            const UINT x=UINT(sx),y=UINT(sy);
            uint8_t* dest=pixels.data()+(size_t(py)*outputWidth_+size_t(px))*4;
            if(shape_.Type==DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MONOCHROME) {
                const unsigned bit=7-(x&7);
                const uint8_t andMask=(pointerBytes_[size_t(y)*shape_.Pitch+x/8]>>bit)&1?255:0;
                const uint8_t xorMask=(pointerBytes_[size_t(y+height)*shape_.Pitch+x/8]>>bit)&1?255:0;
                for(unsigned c=0;c<3;++c)dest[c]=(dest[c]&andMask)^xorMask;
            } else {
                const uint8_t* src=pointerBytes_.data()+size_t(y)*shape_.Pitch+size_t(x)*4;
                for(unsigned c=0;c<3;++c) {
                    if(shape_.Type==DXGI_OUTDUPL_POINTER_SHAPE_TYPE_MASKED_COLOR)dest[c]=src[3]?dest[c]^src[c]:src[c];
                    else dest[c]=uint8_t((unsigned(src[c])*src[3]+unsigned(dest[c])*(255-src[3])+127)/255);
                }
            }
            dest[3]=255;
        }
        auto composed=gpu_.texture(outputWidth_,outputHeight_,DXGI_FORMAT_B8G8R8A8_UNORM);
        gpu_.context->UpdateSubresource(composed.Get(),0,nullptr,pixels.data(),outputWidth_*4,0);return composed;
    }
};
Bytes patternPixels(UINT w,UINT h,uint64_t frame,uint64_t us,bool noise) {
    Bytes pixels(wire::rgbaSize(w,h));const UINT band=std::max(32u,h/6);
    uint32_t random=uint32_t(frame)^0x243f6a88u;
    for(UINT y=0;y<h;++y)for(UINT x=0;x<w;++x) {
        uint8_t red=0,green=0,blue=0;
        if(y<band) {
            red=green=blue=80;
            if(noise&&x<w/2) {random^=random<<13;random^=random>>17;random^=random<<5;red=uint8_t(random);green=uint8_t(random>>8);blue=uint8_t(random>>16);}
            if(x>=(frame*7)%w&&x<std::min<uint64_t>(w,(frame*7)%w+12))red=green=blue=240;
        } else if(y<band+(h-band)/2) {if(x<w/2)red=255;else green=255;}
        else if(x<w/2)blue=255;else red=green=blue=255;
        const size_t p=(size_t(y)*w+x)*4;pixels[p]=blue;pixels[p+1]=green;pixels[p+2]=red;pixels[p+3]=255;
    }
    // Timestamp in readable 3x5 decimal glyphs, plus a 64-bit stripe for automated inspection.
    constexpr uint16_t glyph[]={0x7b6f,0x2492,0x73e7,0x73cf,0x5bc9,0x79cf,0x79ef,0x7249,0x7bef,0x7bcf};
    auto white=[&](UINT x,UINT y){if(x<w&&y<band){const size_t p=(size_t(y)*w+x)*4;pixels[p]=pixels[p+1]=pixels[p+2]=255;}};
    auto digits=std::to_string(us);
    for(size_t k=0;k<digits.size();++k)for(UINT y=0;y<5;++y)for(UINT x=0;x<3;++x)
        if(glyph[digits[k]-'0']&(1u<<(14-y*3-x)))white(UINT(4+k*4+x),4+y);
    for(UINT bit=0;bit<64;++bit)if((us>>bit)&1)for(UINT y=12;y<16;++y)for(UINT x=0;x<2;++x)white(4+bit*2+x,y);
    return pixels;
}
ComPtr<ID3D11Texture2D> patternSurface(Gpu& gpu,UINT w,UINT h,uint64_t frame,uint64_t us,bool noise) {
    auto pixels=patternPixels(w,h,frame,us,noise);auto surface=gpu.texture(w,h,DXGI_FORMAT_B8G8R8A8_UNORM);
    gpu.context->UpdateSubresource(surface.Get(),0,nullptr,pixels.data(),w*4,0);return surface;
}
std::string verifyDecoder(State& state,Gpu& gpu) {
    Decoder decoder(state,gpu);unsigned count=0;
    decoder.onFrame=[&](uint64_t us,UINT w,UINT h,Bytes pixels) {
        if(us!=123456789||w!=320||h!=180||pixels.size()!=size_t(w)*h*4)
            throw Error("hw_decoder_unavailable","Synthetic decoder self-test returned an invalid picture",2);
        const size_t center=(size_t(h/2)*w+w/2)*4;
        if(pixels[center]<160||pixels[center+1]>100||pixels[center+2]>100||pixels[center+3]!=255)
            throw Error("hw_decoder_unavailable","Synthetic decoder self-test failed color validation",2);
        ++count;
    };
    decoder.decode(Bytes(std::begin(DecoderFixture),std::end(DecoderFixture)),123456789);decoder.finish();
    if(count!=1||!decoder.verified)throw Error("hw_decoder_unavailable","Synthetic decoder self-test produced no GPU picture",2);
    return decoder.evidence()+"; synthetic 320x180 IDR decoded, crop and red pixel verified";
}
std::string bounds(const RECT& r) {
    return "{\"x\":"+std::to_string(r.left)+",\"y\":"+std::to_string(r.top)+",\"width\":"+std::to_string(r.right-r.left)+",\"height\":"+std::to_string(r.bottom-r.top)+"}";
}
std::string displayJSON(const Display& d) {
    std::string name;for(const wchar_t* p=d.desc.DeviceName;*p;++p)name+=char(*p);
    DEVMODEW mode{};mode.dmSize=sizeof(mode);double hz=0;
    if(EnumDisplaySettingsW(d.desc.DeviceName,ENUM_CURRENT_SETTINGS,&mode)&&mode.dmDisplayFrequency>1)hz=mode.dmDisplayFrequency;
    std::ostringstream s;s<<"{\"id\":"<<d.id<<",\"deviceName\":"<<wire::quote(name)<<",\"main\":"<<(d.main?"true":"false")
        <<",\"pixelWidth\":"<<d.width<<",\"pixelHeight\":"<<d.height<<",\"pointWidth\":"<<d.width<<",\"pointHeight\":"<<d.height
        <<",\"pixelBounds\":"<<bounds(d.desc.DesktopCoordinates)<<",\"pointBounds\":"<<bounds(d.desc.DesktopCoordinates)
        <<",\"coordinateSpace\":\"physicalPixels\",\"refreshRate\":"<<hz<<"}";return s.str();
}
void probe(State& state,Writer& writer) {
    bool enc=false,dec=false,low=false,standard=false;std::string encID,encEvidence,decEvidence,encError,decError;
    Options o;o.mode="pattern";o.width=320;o.height=180;o.bitrate=1000000;o.frames=1;
    Timebase time;const auto deadline=Clock::now()+8s;
    for(auto& adapter:adapters()) {
        if(state.stop||Clock::now()>deadline)break;
        try {
            Gpu gpu(adapter);
            if(!enc) {
                for(const char* rate:{"lowlatency","standard"}) {
                    try {
                        o.rate=rate;auto encoder=makeEncoder(state,gpu,o);unsigned count=0;
                        encoder->onEncoded=[&](Encoded e){if(!count&&!e.idr)throw Error("encoder_format","Synthetic encoder self-test did not begin with IDR");++count;};
                        const uint64_t firstUs=time.micros(time.now());
                        for(unsigned frame=0;frame<8;++frame){
                            const uint64_t us=firstUs+uint64_t(frame)*1000000/o.fps;
                            auto surface=patternSurface(gpu,o.width,o.height,frame,us,false);
                            encoder->encode(surface.Get(),us);
                        }
                        encoder->finish();
                        if(count!=8||!encoder->verified)throw Error("hw_encoder_unavailable","Synthetic encoder self-test discarded a streaming access unit",2);
                        if(!enc){encID=encoder->encoderID;encEvidence=encoder->evidence();}
                        enc=true;
                        if(std::string(rate)=="lowlatency")low=encoder->lowLatency;else standard=true;
                    } catch(const Error& e){encError=e.code;log("probe_encoder_unavailable code="+e.code);}
                }
            }
            if(!dec)try {decEvidence=verifyDecoder(state,gpu);dec=true;}catch(const Error& e){decError=e.code;log("probe_decoder_unavailable code="+e.code);}
        } catch(const Error& e){log("probe_adapter_unavailable code="+e.code);}
        if(enc&&dec)break;
    }
    const auto monitors=displays();auto restriction=desktopRestriction();
    if(restriction.empty()) {
        restriction="no_attached_dxgi_display";
        for(const auto& display:monitors) {
            try {
                Gpu preflight(display.adapter);ComPtr<IDXGIOutputDuplication> duplication;
                HRESULT hr=display.output->DuplicateOutput(preflight.device.Get(),&duplication);
                if(SUCCEEDED(hr)){restriction.clear();break;}
                restriction="dxgi_preflight_failed_"+hex32(uint32_t(hr));
            }catch(const Error&){restriction="dxgi_preflight_device_unavailable";}
        }
    }
    std::ostringstream s;s<<"{\"protocol\":1,\"mode\":\"probe\",\"codec\":\"h264\",\"platform\":\"windows\",\"hardware\":"<<(enc&&dec?"true":"false")
        <<",\"hardwareEncode\":"<<(enc?"true":"false")<<",\"hardwareDecode\":"<<(dec?"true":"false")
        <<",\"lowLatencyRateControl\":"<<(low?"true":"false")<<",\"standardRateControlHardware\":"<<(standard?"true":"false")
        <<",\"encoderID\":"<<wire::quote(encID)<<",\"hardwareEvidence\":"<<wire::quote(encEvidence)<<",\"decoderHardwareEvidence\":"<<wire::quote(decEvidence)
        <<",\"screenCaptureAccess\":"<<wire::quote(restriction.empty()?"granted":"denied")<<",\"captureRestriction\":"<<wire::quote(restriction)
        <<",\"captureAuthorization\":\"host_user_display_selection\",\"permissionPromptIssued\":false,\"displayIdSource\":\"DXGI_OUTPUT_DESC.DeviceName DISPLAY suffix\""
        <<",\"encoderError\":"<<wire::quote(enc?"":encError)<<",\"decoderError\":"<<wire::quote(dec?"":decError)<<",\"displays\":[";
    for(size_t i=0;i<monitors.size();++i){if(i)s<<',';s<<displayJSON(monitors[i]);}s<<"]}";
    writer.control(wire::json(1,s.str()));state.shutdown();
}
std::string encodeHello(const Options& o,Encoder& encoder,const Display* display) {
    auto dimensions=wire::sps(encoder.parameters.spsBytes);
    std::ostringstream s;s<<"{\"protocol\":1,\"mode\":"<<wire::quote(o.mode)<<",\"codec\":\"h264\",\"hardware\":true,\"platform\":\"windows\""
        <<",\"width\":"<<dimensions.width<<",\"height\":"<<dimensions.height<<",\"fps\":"<<o.fps<<",\"bitrate\":"<<encoder.currentBitrate()
        <<",\"profileLevelId\":"<<wire::quote(encoder.parameters.profileLevelId())<<",\"keyframeIntervalSec\":"<<o.keyInterval
        <<",\"lowLatencyRateControl\":"<<(encoder.lowLatency?"true":"false")<<",\"encoderID\":"<<wire::quote(encoder.encoderID)
        <<",\"hardwareEvidence\":"<<wire::quote(encoder.evidence());
    if(display)s<<",\"displayID\":"<<display->id<<",\"sourcePixelWidth\":"<<display->width<<",\"sourcePixelHeight\":"<<display->height;
    else s<<",\"frames\":"<<o.frames;
    s<<"}";return s.str();
}
void encodeMode(State& state,Writer& writer,Options o) {
    Reader reader(state,false,o.mode=="capture");Timebase time;
    std::unique_ptr<Gpu> gpu;std::unique_ptr<Encoder> encoder;std::unique_ptr<Capture> capture;
    Display selected;const Display* display=nullptr;TerminalUnwind unwind(state);
    if(o.mode=="capture") {
        auto list=displays();auto it=std::find_if(list.begin(),list.end(),[&](const Display& d){return o.display?d.id==o.display:d.main;});
        if(it==list.end())throw Error("display_not_found","Selected DISPLAY numeric ID is not attached to this session",4);
        selected=*it;display=&selected;requireDesktop();
        const double scale=std::min({1.0,double(o.width)/selected.width,double(o.height)/selected.height});
        o.width=uint32_t(selected.width*scale)&~1u;o.height=uint32_t(selected.height*scale)&~1u;
        if(o.width<64||o.height<64)throw Error("capture_unavailable","Selected display is too small for hardware H264",4);
        gpu=std::make_unique<Gpu>(selected.adapter);encoder=makeEncoder(state,*gpu,o);capture=std::make_unique<Capture>(state,*gpu,selected,o.width,o.height);
    } else {
        for(auto& a:adapters()) {
            try {auto candidate=std::make_unique<Gpu>(a);auto enc=makeEncoder(state,*candidate,o);gpu=std::move(candidate);encoder=std::move(enc);break;}
            catch(const Error& e){log("pattern_adapter_rejected code="+e.code);}
        }
        if(!encoder)throw Error("hw_encoder_unavailable","No hardware adapter can encode the requested synthetic pattern",2);
    }
    bool hello=false;
    encoder->onEncoded=[&](Encoded e) {
        if(!hello){writer.control(wire::json(1,encodeHello(o,*encoder,display)));hello=true;state.startupDeadline=0;}
        Record r{2,{}};r.payload.reserve(9+e.data.size());wire::be64(r.payload,e.unixUs);r.payload.push_back(e.idr?1:0);
        r.payload.insert(r.payload.end(),e.data.begin(),e.data.end());writer.media(std::move(r),e.idr);
    };
    const auto started=Clock::now();auto lastStats=started,lastEvent=started-1s;
    uint64_t n=0,lastUs=0;int64_t next=time.now();bool dirty=false;
    while(!state.stop) {
        encoder->pump();
        if(o.mode=="pattern") {
            if(o.frames&&n>=o.frames)break;
            if(!o.noPace&&time.now()<next){Sleep(1);continue;}
            const uint64_t us=std::max(lastUs+1,time.micros(time.now()));
            auto surface=patternSurface(*gpu,o.width,o.height,n,us,o.noise);
            if(!encoder->encode(surface.Get(),us))break;
            lastUs=us;++n;next=std::max(next+time.frequency/o.fps,time.now());
        } else {
            if(capture->acquire(time))dirty=true;
            const int64_t now=time.now();const bool forced=state.forceIDR.load();
            if(capture->hasImage()&&((dirty&&now>=next)||forced)) {
                requireDesktop();auto surface=capture->picture(!o.noCursor);
                const uint64_t us=std::max(lastUs+1,forced&&!dirty?time.micros(now):capture->timestamp);
                if(!encoder->encode(surface.Get(),us))break;
                lastUs=us;++n;dirty=false;next=now+time.frequency/o.fps;
            }
            if(capture->initialFrameTimedOut())throw Error("capture_start_failed","Desktop duplication delivered no image",4);
        }
        if(hello&&o.events&&state.forceIDR&&Clock::now()-lastEvent>=200ms) {
            writer.control(wire::json(4,"{\"event\":\"keyframe_requested\",\"reason\":\"control_or_backpressure\"}"));lastEvent=Clock::now();
        }
        if(Clock::now()-lastStats>5s){log("stats encoded="+std::to_string(encoder->outputCount)+" droppedOutput="+std::to_string(writer.dropped.load()));lastStats=Clock::now();}
    }
    state.shutdown();reader.close();encoder->finish();
    log("final-stats encoded="+std::to_string(encoder->outputCount)+" droppedOutput="+std::to_string(writer.dropped.load()));
}
void decodeMode(State& state,Writer& writer,const Options& o) {
    std::unique_ptr<Gpu> gpu;std::string evidence;
    for(auto& a:adapters()) {
        try {auto candidate=std::make_unique<Gpu>(a);evidence=verifyDecoder(state,*candidate);gpu=std::move(candidate);break;}
        catch(const Error& e){log("decoder_adapter_rejected code="+e.code);}
    }
    if(!gpu)throw Error("hw_decoder_unavailable","No adapter passed the actual GPU decoder self-test",2);
    writer.control(wire::json(1,"{\"protocol\":1,\"mode\":\"decode\",\"codec\":\"h264\",\"hardware\":true,\"platform\":\"windows\",\"output\":\"rgba\",\"decoderID\":\"CLSID_CMSH264DecoderMFT\",\"hardwareEvidence\":"+wire::quote(evidence)+"}"));
    Decoder decoder(state,*gpu);
    decoder.onFrame=[&](uint64_t us,UINT w,UINT h,Bytes pixels) {
        Record r{3,{}};r.payload.reserve(pixels.size()+16);wire::be64(r.payload,us);wire::be32(r.payload,w);wire::be32(r.payload,h);
        r.payload.insert(r.payload.end(),pixels.begin(),pixels.end());writer.media(std::move(r),false,o.policy=="block");
    };
    decoder.onNeedIDR=[&](const char* reason){if(o.events)writer.control(wire::json(4,"{\"event\":\"need_keyframe\",\"reason\":"+wire::quote(reason)+"}"));};
    Reader reader(state,true,false);TerminalUnwind unwind(state);state.startupDeadline=0;
    while(!state.stop) {
        decoder.pump();Record r;bool eof=false;
        if(!reader.pop(r,eof)){if(eof)break;continue;}
        if(r.type!=2){if(r.type!=1&&r.type!=4&&r.type!=127)log("unknown_record_skipped");continue;}
        if(r.payload.size()<10||r.payload[8]>1)throw Error("decoder_format","Invalid H264 record prefix",6);
        const uint64_t us=wire::readBE(r.payload.data(),8);Bytes data(r.payload.begin()+9,r.payload.end());
        const auto nals=wire::annex(data);const bool idr=std::any_of(nals.begin(),nals.end(),[](const Bytes& n){return (n[0]&31)==5;});
        if(idr!=(r.payload[8]!=0))throw Error("decoder_format","H264 keyframe flag does not match IDR NALs",6);
        decoder.decode(data,us);
    }
    state.shutdown();reader.close();decoder.finish();
    log("final-stats decoded="+std::to_string(decoder.outputCount)+" droppedOutput="+std::to_string(writer.dropped.load()));
}
void run(State& state,Writer& writer,const Options& o) {
    state.startupDeadline=GetTickCount64()+8500;
    Runtime runtime(state);
    if(o.mode=="probe")probe(state,writer);
    else if(o.mode=="decode")decodeMode(state,writer,o);
    else encodeMode(state,writer,o);
    state.startupDeadline=0;state.shutdown();
}
} // namespace
int main(int argc,char** argv) {
    // DPI awareness keeps DXGI and GetMonitorInfo bounds in the physical coordinate space used by SendInput.
    using DpiContext=BOOL(WINAPI*)(HANDLE);
    auto dpi=reinterpret_cast<DpiContext>(GetProcAddress(GetModuleHandleW(L"user32.dll"),"SetProcessDpiAwarenessContext"));
    if(dpi)dpi(reinterpret_cast<HANDLE>(-4));else SetProcessDPIAware();
    State state;signalState=&state;SetConsoleCtrlHandler(consoleHandler,TRUE);Watchdog watchdog(state);
    std::unique_ptr<Writer> writer;int exitCode=0;
    try {auto o=options(argc,argv);writer=std::make_unique<Writer>(state,o.queue);run(state,*writer,o);}
    catch(const Error& e) {
        state.startupDeadline=0;state.shutdown();state.stop=true;exitCode=e.exitCode;
        log("failure code="+e.code);
        if(!writer)writer=std::make_unique<Writer>(state,2);
        if(!state.ioFailed)writer->control(wire::json(127,"{\"code\":"+wire::quote(e.code)+",\"message\":"+wire::quote(e.what())+"}"));
    } catch(const std::exception&) {
        state.startupDeadline=0;state.shutdown();state.stop=true;exitCode=1;log("failure code=internal");
        if(!writer)writer=std::make_unique<Writer>(state,2);
        if(!state.ioFailed)writer->control(wire::json(127,"{\"code\":\"internal\",\"message\":\"Unhandled helper failure\"}"));
    }
    state.shutdown();if(writer)writer->finish();if(state.ioFailed)exitCode=5;
    SetConsoleCtrlHandler(consoleHandler,FALSE);signalState=nullptr;return exitCode;
}
