# Dependency License Compatibility Matrix

> Reference for the dependency-auditor (Mode 3). Adapted from the upstream
> claude-skills engineering/dependency-auditor license matrix. **Guidance, not legal
> advice** — escalate GPL/AGPL-in-proprietary questions to whoever owns licensing in your organization.

## Understanding License Types

### Permissive Licenses
- **MIT License**: Very permissive, allows commercial use, modification, and distribution
- **Apache 2.0**: Permissive with patent grant and trademark restrictions
- **BSD 3-Clause**: Permissive with non-endorsement clause
- **BSD 2-Clause**: Simple permissive license
- **ISC License**: Functionally equivalent to MIT

### Weak Copyleft Licenses
- **LGPL 2.1/3.0**: Library-level copyleft, allows linking but requires modifications to be shared
- **MPL 2.0**: File-level copyleft, compatible with many licenses

### Strong Copyleft Licenses
- **GPL 2.0/3.0**: Requires entire derivative work to be GPL-licensed
- **AGPL 3.0**: Extends GPL to network services (SaaS applications)

## Compatibility Matrix

| Project License | MIT | Apache-2.0 | BSD-3 | LGPL-2.1 | LGPL-3.0 | MPL-2.0 | GPL-2.0 | GPL-3.0 | AGPL-3.0 |
|----------------|-----|------------|-------|----------|----------|---------|---------|---------|----------|
| **MIT**        | ✅   | ✅          | ✅     | ⚠️        | ⚠️        | ⚠️       | ❌       | ❌       | ❌        |
| **Apache-2.0** | ✅   | ✅          | ✅     | ❌        | ⚠️        | ✅       | ❌       | ⚠️       | ⚠️        |
| **BSD-3**      | ✅   | ✅          | ✅     | ⚠️        | ⚠️        | ⚠️       | ❌       | ❌       | ❌        |
| **LGPL-2.1**   | ✅   | ❌          | ✅     | ✅        | ❌        | ❌       | ✅       | ❌       | ❌        |
| **LGPL-3.0**   | ✅   | ✅          | ✅     | ❌        | ✅        | ✅       | ❌       | ✅       | ✅        |
| **MPL-2.0**    | ✅   | ✅          | ✅     | ❌        | ✅        | ✅       | ❌       | ✅       | ✅        |
| **GPL-2.0**    | ✅   | ❌          | ✅     | ✅        | ❌        | ❌       | ✅       | ❌       | ❌        |
| **GPL-3.0**    | ✅   | ✅          | ✅     | ❌        | ✅        | ✅       | ❌       | ✅       | ✅        |
| **AGPL-3.0**   | ✅   | ✅          | ✅     | ❌        | ✅        | ✅       | ❌       | ✅       | ✅        |

**Legend:**
- ✅ Generally Compatible
- ⚠️ Compatible with conditions/restrictions
- ❌ Incompatible

## Detailed Compatibility Rules

### MIT Project with Other Licenses

**Compatible:**
- MIT, Apache-2.0, BSD (all variants), ISC: Full compatibility
- LGPL 2.1/3.0: Can use LGPL libraries via dynamic linking
- MPL 2.0: Can use MPL modules, must keep MPL files under MPL

**Incompatible:**
- GPL 2.0/3.0: GPL requires entire project to be GPL
- AGPL 3.0: AGPL extends to network services

### Apache 2.0 Project with Other Licenses

**Compatible:**
- MIT, BSD, ISC: Full compatibility
- LGPL 3.0: Compatible (LGPL 3.0 has Apache compatibility clause)
- MPL 2.0: Compatible
- GPL 3.0: Compatible (GPL 3.0 has Apache compatibility clause)

**Incompatible:**
- LGPL 2.1: License incompatibility
- GPL 2.0: License incompatibility (no Apache clause)

### GPL Projects

**GPL 2.0 Compatible:**
- MIT, BSD, ISC: Can incorporate permissive code
- LGPL 2.1: Compatible
- Other GPL 2.0: Compatible

**GPL 2.0 Incompatible:**
- Apache 2.0: Different patent clauses
- LGPL 3.0: Version incompatibility
- GPL 3.0: Version incompatibility

**GPL 3.0 Compatible:**
- All permissive licenses (MIT, Apache, BSD, ISC)
- LGPL 3.0: Version compatibility
- MPL 2.0: Explicit compatibility

## Common Compatibility Scenarios

### Scenario 1: Permissive Project with GPL Dependency
**Problem:** MIT-licensed project wants to use GPL library
**Impact:** Entire project must become GPL-licensed
**Solutions:**
1. Find alternative non-GPL library
2. Use dynamic linking (if possible)
3. Change project license to GPL
4. Remove the dependency

### Scenario 2: Apache Project with GPL 2.0 Dependency
**Problem:** Apache 2.0 project with GPL 2.0 dependency
**Impact:** License incompatibility due to patent clauses
**Solutions:**
1. Upgrade to GPL 3.0 if available
2. Find alternative library
3. Use via separate service (API boundary)

### Scenario 3: Commercial Product with AGPL Dependency
**Problem:** Proprietary software using AGPL library
**Impact:** AGPL copyleft extends to network services
**Solutions:**
1. Obtain commercial license
2. Replace with permissive alternative
3. Use via separate service with API boundary
4. Make entire application AGPL

## License Combination Rules

### Safe Combinations
1. **Permissive + Permissive**: Always safe
2. **Permissive + Weak Copyleft**: Usually safe with proper attribution
3. **GPL + Compatible Permissive**: Safe, result is GPL

### Risky Combinations
1. **Apache 2.0 + GPL 2.0**: Incompatible patent terms
2. **Different GPL versions**: Version compatibility issues
