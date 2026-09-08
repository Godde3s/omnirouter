# 🌐 OmniRouter — یک روتر برای همه‌ی مدل‌ها

<div align="center">

**GLM · Qwen · DeepSeek · هر API دیگه — با یک کلید، یک لاگین، یک آدرس**

کیفیت واقعی API روی وبِ رایگانِ چت‌بات‌ها + ارائه‌دهنده‌های سفارشی + داشبورد کامل

`OpenAI API` · `Anthropic API` · `SSE استریم` · `Tool Calling` · `Failover خودکار`

</div>

---

## این چیه؟

OmniRouter سه بریج اثبات‌شده‌ی **glm-free-api**، **qwen-free-api** و **deepseek-free-api** را به‌عنوان کتابخانه داخل خودش دارد و روی آن‌ها یک لایه‌ی روتر کامل می‌سازد — دقیقاً همان کاری که ۹router برای APIهای رسمی می‌کند، اما برای **وبِ رایگان** مدل‌ها:

| قابلیت | وضعیت |
|---|---|
| یک کلید برای همه‌ی مدل‌ها (`sk-…`) | ✅ |
| `/v1/chat/completions` سازگار با OpenAI + استریم SSE | ✅ |
| `/v1/messages` سازگار با Anthropic (Claude Code, Cline) | ✅ |
| Tool calling کامل (چرخه‌ی R1→R2) برای Hermes/OpenCode/Cline | ✅ |
| مدل `auto` با زنجیره‌ی failover بین ارائه‌دهنده‌ها | ✅ |
| اضافه‌کردن API دلخواه (Gemini AI Studio، OpenRouter، Groq، …) از داشبورد | ✅ |
| داشبورد فارسی RTL با وضعیت زنده، کلیدها، لاگ‌ها | ✅ |
| تک‌فایل، بدون وابستگی — لینوکس / ویندوز / مک / داکر | ✅ |

## شروع سریع (۳۰ ثانیه)

```bash
# ۱) دانلود باینری از Releases (یا بیلد با Go)
./start.sh

# ۲) اولین اجرا خودش .env می‌سازد — حداقل یک اعتبار بگذار:
#    DEEPSEEK_TOKENS=...   ← از ./ds-login
#    QWEN_TOKENS=...       ← اختیاری (مهمان هم کار می‌کند)
#    ZAI_TOKENS=...        ← اختیاری (مهمان هم کار می‌کند)

# ۳) داشبورد:  http://localhost:8080   (رمز پیش‌فرض: admin)
```

### اتصال کلاینت‌ها

**OpenAI سازگار (OpenCode / Hermes / Cline / هر کلاینت):**
```bash
Base URL:  http://localhost:8080/v1
API Key:   sk-…        (از داشبورد)
Model:     auto        ← بهترین ارائه‌دهنده‌ی سالم، با failover
```

**Anthropic سازگار (Claude Code و…):**
```bash
ANTHROPIC_BASE_URL=http://localhost:8080
ANTHROPIC_API_KEY=sk-…
```

```python
from openai import OpenAI
client = OpenAI(base_url="http://localhost:8080/v1", api_key="sk-…")
client.chat.completions.create(model="auto", messages=[{"role":"user","content":"سلام"}])
```

## مدل‌ها

| ارائه‌دهنده | مدل‌ها | ورود |
|---|---|---|
| **Qwen** (chat.qwen.ai) | `qwen3.8-max` (پرچم‌دار) · `qwen3.7-plus` | مهمان یا توکن |
| **DeepSeek** (chat.deepseek.com) | `deepseek-chat` · `deepseek-reasoner` · `deepseek-expert` | توکن الزامی |
| **GLM** (chat.z.ai) | `GLM-5.1` · `GLM-5` + کشف خودکار | مهمان یا توکن |
| **سفارشی** | هر چیزی که خودت اضافه کنی (مثلاً `gemini-2.5-pro` با کلید رایگان AI Studio) | کلید خودت |

آدرس‌دهی: `auto` (زنجیره) · `qwen/qwen3.8-max` (صریح) · خودِ نام مدل (کاتالوگ مشترک)

## داشبورد

- **نمای کلی**: سلامت زنده‌ی هر ارائه‌دهنده، آمار کلیدها و خطاها
- **کلیدها**: ساخت/حذف/محدودسازی کلیدهای `sk-…` برای کلاینت‌های مختلف
- **ارائه‌دهنده‌ها**: افزودن API سازگار با OpenAI با پیش‌تنظیم آماده (✨ Gemini AI Studio)
- **لاگ‌ها**: ۵۰۰ درخواست آخر با مدل، ارائه‌دهنده، وضعیت و تأخیر

## حالت‌های مهمان و توکن‌ها

| سرویس | بدون توکن | با توکن |
|---|---|---|
| Qwen | مهمان (روی IP خانگی؛ روی سرور یک‌بار `./qwen-bx`) | پول چنداکانتی + استریم پایدارتر |
| GLM | مهمان (نیاز به دیوایس‌توکن کپچا در `tokens.sqlite` یا `ZAI_TOKENS`) | پول چنداکانتی |
| DeepSeek | ❌ ندارد | توکن الزامی (`./ds-login`) |

ابزارهای کمکی (اختیاری، هرکدام یک‌بار اجرا می‌شوند):
```bash
./ds-login     # گرفتن توکن دیپ‌سیک با مرورگر
./qwen-bx      # هدرهای Baxia برای حالت مهمان قوِن روی IP دیتاسنتر
```

## متغیرهای محیطی (خلاصه)

```env
PORT=8080                 # پورت عمومی روتر
ADMIN_PASSWORD=admin      # رمز داشبورد — عوضش کن!
ROUTER_KEY=               # کلید اول (خالی = تولید خودکار)
AUTO_CHAIN=qwen/qwen3.8-max,ds/deepseek-chat,glm/GLM-5.1
AGENT_MODE=1              # شیم tool-calling برای ایجنت‌ها

QWEN_TOKENS=  QWEN_BX_FILE=qwen-bx.json
ZAI_TOKENS=   GLM_TOKEN_DB=tokens.sqlite
DEEPSEEK_TOKENS=
AUTH_TOKEN=omni-internal-change-me   # راز داخلی بین هسته و بریج‌ها
```

## داکر

```bash
cp .env.example .env   # اعتبارها را بگذار
docker compose up -d   # http://localhost:8080
```

## معماری

```
کلاینت (OpenCode/Hermes/Cline/…)
        │  یک کلید sk-…
        ▼
┌─────────────────────────── OmniRouter :8080 ───────────────────────────┐
│  /v1/chat/completions   /v1/messages   /v1/models   /dashboard        │
│  auth کلید → کاتالوگ مدل‌ها → resolve → زنجیره‌ی failover               │
└──────┬───────────────────────┬───────────────────────┬─────────────────┘
       ▼                       ▼                       ▼
  qbridge (Qwen)         zbridge (GLM)          dsbridge (DeepSeek)
  guest/bx + pool        guest/captcha + pool   PoW(wazero) + pool
       ▼                       ▼                       ▼
  chat.qwen.ai             chat.z.ai           chat.deepseek.com
       + ارائه‌دهنده‌های سفارشی (هر endpoint سازگار با OpenAI)
```

- **bridges**: همان کلاینت‌های اثبات‌شده‌ی سه پروژه‌ی قبلی، دست‌نخورده
- **dsbridge**: پورت کامل Go از کلاینت دیپ‌سیک — حل PoW با اجرای **همان wasm رسمی دیپ‌سیک** داخل wazero (بدون CGO)
- **core**: کاتالوگ زنده، failover بین ارائه‌دهنده‌ها (قبل از اولین بایتِ استریم)، کلیدها، لاگ

## عیب‌یابی

| خطا | معنی و راه‌حل |
|---|---|
| `model not found on any provider` | مدل را در داشبورد → مدل‌ها ببین؛ یا `provider/model` بنویس |
| `هیچ اکانت DeepSeek ثبت نشده` | `./ds-login` را اجرا کن و `DEEPSEEK_TOKENS` را پر کن |
| `captcha verification rejected` (GLM) | دیوایس‌توکن‌های `tokens.sqlite` منقضی شده‌اند — `ZAI_TOKENS` بگذار یا توکن‌ها را تازه کن |
| `unauthorized — refresh the token` (Qwen) | توکن‌های `QWEN_TOKENS` منقضی‌اند؛ یا یک‌بار `./qwen-bx` برای مهمان |
| داشبورد باز نمی‌شود | رمز `ADMIN_PASSWORD` را در `.env` چک کن |

## English Summary

OmniRouter embeds the proven **glm-free-api**, **qwen-free-api** and **deepseek-free-api** bridges as libraries behind one OpenAI/Anthropic-compatible gateway: one `sk-…` key, live model catalog, cross-provider failover (`auto`), custom OpenAI-compatible providers managed from a Persian-RTL dashboard, full tool-calling for coding agents (OpenCode, Hermes, Cline). DeepSeek's proof-of-work is solved natively by executing DeepSeek's own WASM via wazero — pure Go, zero CGO. Single static binary for Linux / Windows / macOS / Docker. See `.env.example` and the troubleshooting table above.

## License

MIT — مصرف شخصی و آموزشی. با احترام به شرایط سرویس‌های آپستریم، مسئولیت استفاده با خود شماست.
