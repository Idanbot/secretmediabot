# Security Policy

## Supported Versions

Only the latest release and the current `main` branch are actively supported with security updates.

| Version | Supported          |
| ------- | ------------------ |
| main    | :white_check_mark: |
| < 1.0   | :x:                |

## Reporting a Vulnerability

If you discover a security vulnerability in Secret Media Bot:

1. **Do NOT open a public GitHub issue.**
2. Send an email with a detailed description of the vulnerability, reproduction steps, and potential impact to the maintainer or use GitHub Private Vulnerability Reporting on this repository.
3. You will receive an acknowledgement within 48 hours with next steps.
4. We coordinate a remediation and a responsible disclosure timeline before publishing details or fixes.

## Security Design & Defense in Depth

- **End-to-End Persistence Security**: Plaintext secrets and media are encrypted with authenticated AES-256-GCM before writing to PostgreSQL.
- **Ephemeral Delivery**: Secrets delivered via Telegram Bot API ephemeral messages are tracked and scheduled for deletion after viewing.
- **Audit & Rate Limiting**: All sensitive operations (opening, deletion, claims) are rate-limited and audited without storing plaintext key material or user secrets in audit logs.
- **Least Privilege**: The container runs under a non-root user (`65532:65532`) with all Linux capabilities dropped (`cap_drop: [ALL]`) and `read_only: true` filesystem.
