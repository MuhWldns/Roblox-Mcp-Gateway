# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

**Primary:** Solo Roblox game developer (indie) yang menggunakan AI tools (ChatGPT, Claude) untuk membantu coding di Roblox Studio. Mereka butuh menghubungkan AI assistant ke Studio tanpa setup networking yang rumit.

## Product Purpose

RobloxKit menghubungkan ChatGPT dan Claude ke Roblox Studio melalui gateway cloud yang aman. Developer install Bridge di PC mereka, login via web dashboard, dan AI tools langsung bisa mengontrol Roblox Studio — tanpa port forwarding, tanpa expose Studio ke internet.

## Positioning

Satu-satunya MCP gateway untuk Roblox Studio yang bekerja di balik NAT/CGNAT tanpa port forwarding. Bridge outbound-only, tidak ada inbound connection ke PC user.

## Operating Context

- Developer membuka Roblox Studio dan menjalankan RobloxBridge.exe
- Login via web dashboard di browser
- Setup connector di ChatGPT/Claude dengan MCP endpoint
- Monitor device, studio sessions, connectors, dan license dari dashboard
- Admin melakukan transfer device, account recovery, dan trial extension

## Capabilities and Constraints

- React + Vite + TypeScript frontend
- Go backend modular monolith
- MySQL 8.0+ database
- Single-instance MVP (tidak horizontal scaling)
- PM2 fork mode, instances: 1
- Bridge: Windows executable, outbound WSS only
- Roblox OAuth untuk login, MCP OAuth untuk AI clients
- License per device, 14-day free trial

## Brand Commitments

- Nama: **Roblox Kit BY RBX**
- Belum ada logo, palet, atau aset visual lain
- Terminology: user-facing onboarding says "Connect your PC" and "pairing code"; "enrollment"/"enroll" is internal/API terminology only. Public landing: `/` (Home) is public; dashboard sections live under the shell.

## Evidence on Hand

- PRD v3.1 lengkap (PRD.md)
- Backend Go sudah production-ready
- Frontend fungsional (semua route, API client, state management) tanpa styling
- PM2 deployment di VPS production

## Product Principles

1. **No inbound connection** — Bridge outbound-only, user PC tidak pernah expose port
2. **Separation of credentials** — Roblox OAuth, web session, device credential, MCP OAuth terpisah
3. **Dashboard is the product** — semua management via web, Bridge hanya status terminal
4. **Single instance simplicity** — MVP tidak pakai distributed systems yang tidak perlu
5. **Developer-first** — UX untuk developer yang technical, bukan end-user casual

## Accessibility & Inclusion

- Target user developer technical, familiar dengan terminal dan AI tools
- Web dashboard harus usable di desktop browser modern