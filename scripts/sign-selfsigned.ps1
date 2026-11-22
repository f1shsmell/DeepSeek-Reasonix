# The Windows signing method for a release that holds no code-signing
# credentials: each release run mints a self-signed code-signing certificate,
# trusts it on the runners that sign and verify, and signs with it. What a
# signature asserts here is integrity and this-run consistency, not publisher
# identity — no CA is involved, so a fresh install shows an unknown publisher.
# The updater reads hashes from the release catalog and is untouched by this.
#
# Four modes, one file: mint generates the certificate and exports the PFX the
# later jobs share; import loads a minted PFX and trusts it on the runner;
# sign-payload and sign-file mirror sign-certum.ps1's entry points so the two
# signing stages read the same way regardless of which method stands behind
# them.

param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('mint', 'import', 'sign-payload', 'sign-file')]
    [string]$Mode,

    # mint: where the PFX, its password and the public certificate land.
    [string]$OutputDirectory,

    # import: a minted PFX and the file holding its password.
    [string]$PfxPath,
    [string]$PasswordFilePath,

    # sign-payload: the directory holding signing-files.txt and the app tree.
    [string]$PayloadDirectory,

    # sign-file: one file, the installer.
    [string]$FilePath
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$script:SigningSubject = 'CN=Reasonix Studio self-built release'

. (Join-Path $PSScriptRoot 'windows-signing-lib.ps1')

switch ($Mode) {
    'mint' {
        if ($OutputDirectory -eq '') { throw 'mint needs -OutputDirectory' }
        New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
        $certificate = New-SelfSignedCertificate `
            -Type CodeSigningCert `
            -Subject $script:SigningSubject `
            -KeyUsage DigitalSignature `
            -KeyUsageProperty Sign `
            -KeyAlgorithm RSA `
            -KeyLength 3072 `
            -HashAlgorithm SHA256 `
            -NotAfter (Get-Date).AddDays(7) `
            -CertStoreLocation Cert:\CurrentUser\My
        # A week outlives every job of one release run; shorter cannot sign a
        # second release if a PFX ever leaks out of the runner.
        #
        # The PFX travels through .NET here, not through the PKI module: the
        # runners' PowerShell 7 PKI module carries no PFX cmdlets.
        $passwordPlain = [guid]::NewGuid().ToString('N')
        $pfxBytes = $certificate.Export([System.Security.Cryptography.X509Certificates.X509ContentType]::Pfx, $passwordPlain)
        [IO.File]::WriteAllBytes((Join-Path $OutputDirectory 'release-signing.pfx'), $pfxBytes)
        Set-Content -LiteralPath (Join-Path $OutputDirectory 'release-signing.password') -Value $passwordPlain -NoNewline
        Set-Content -LiteralPath (Join-Path $OutputDirectory 'release-signing.thumbprint') -Value $certificate.Thumbprint -NoNewline
        Set-Content -LiteralPath (Join-Path $OutputDirectory 'release-signing.subject') -Value $certificate.Subject -NoNewline
        Write-Host "Minted release signing certificate $($certificate.Thumbprint) — $($certificate.Subject)"
    }
    'import' {
        if ($PfxPath -eq '' -or $PasswordFilePath -eq '') { throw 'import needs -PfxPath and -PasswordFilePath' }
        $passwordPlain = (Get-Content -LiteralPath $PasswordFilePath -Raw).Trim()
        $pfxBytes = [IO.File]::ReadAllBytes($PfxPath)
        # Import through .NET because the runners' PowerShell 7 PKI module
        # carries no PFX cmdlets. Two placements, both needed:
        #   CurrentUser\My with a persisted key — where signtool /sha1 finds
        #   the certificate to sign with;
        #   LocalMachine\Root — trusting the minted root, which is what lets
        #   signtool verify call a self-signed chain valid, so the verification
        #   scripts read the self-signed release exactly as they read a
        #   CA-signed one. The runner is ephemeral and administrator.
        $certificate = [System.Security.Cryptography.X509Certificates.X509Certificate2]::new(
            $pfxBytes, $passwordPlain,
            [System.Security.Cryptography.X509Certificates.X509KeyStorageFlags]::PersistKeySet)
        $myStore = [System.Security.Cryptography.X509Certificates.X509Store]::new('My', 'CurrentUser')
        $myStore.Open('ReadWrite')
        $myStore.Add($certificate)
        $myStore.Close()
        $rootStore = [System.Security.Cryptography.X509Certificates.X509Store]::new('Root', 'LocalMachine')
        $rootStore.Open('ReadWrite')
        $rootStore.Add($certificate)
        $rootStore.Close()
        Write-Host "Imported and trusted release signing certificate $($certificate.Thumbprint)"
        "thumbprint=$($certificate.Thumbprint)"
        "subject=$($certificate.Subject)"
    }
    default {
        $thumbprint = $env:SIGN_CERT_THUMBPRINT
        if ($thumbprint -notmatch '^[0-9a-fA-F]{40}$') { throw 'Invalid SIGN_CERT_THUMBPRINT' }
        $files = if ($Mode -eq 'sign-payload') {
            if ($PayloadDirectory -eq '') { throw 'sign-payload needs -PayloadDirectory' }
            $root = (Resolve-Path -LiteralPath $PayloadDirectory).Path
            $prefix = $root.TrimEnd('\', '/') + [IO.Path]::DirectorySeparatorChar
            $entries = @(Get-Content -LiteralPath (Join-Path $root 'signing-files.txt') |
                ForEach-Object { $_.Trim() } | Where-Object { $_ -and -not $_.StartsWith('#') })
            if ($entries.Count -eq 0) { throw 'Empty signing manifest' }
            @($entries | ForEach-Object {
                $candidate = [IO.Path]::GetFullPath((Join-Path $root $_))
                if (-not $candidate.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
                    throw 'Signing manifest path escapes payload directory'
                }
                $candidate
            })
        } else {
            if ($FilePath -eq '') { throw 'sign-file needs -FilePath' }
            @((Resolve-Path -LiteralPath $FilePath).Path)
        }
        foreach ($file in $files) {
            if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { throw "Missing signing input: $file" }
        }
        $tool = Get-SignTool
        foreach ($file in $files) {
            & $tool sign /sha1 $thumbprint /fd SHA256 /tr http://timestamp.digicert.com /td SHA256 $file
            if ($LASTEXITCODE -ne 0) { throw "Authenticode signing failed: $file" }
            Assert-EmbeddedSignature -Path $file -Thumbprint $thumbprint
        }
    }
}
