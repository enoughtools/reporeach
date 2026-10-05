/* Build-time profile authentication with public macOS Security APIs.
 * Certificate marker payloads are checked separately by the Python caller.
 */
#include <CoreFoundation/CoreFoundation.h>
#include <Security/CMSDecoder.h>
#include <Security/Security.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void need(int condition, const char *message) {
    if (!condition) {
        fprintf(stderr, "Profile authentication failed: %s\n", message);
        exit(1);
    }
}

static CFDataRef load(const char *path) {
    FILE *stream = fopen(path, "rb");
    need(stream != NULL, "cannot open input");
    need(fseek(stream, 0, SEEK_END) == 0, "cannot measure input");
    long length = ftell(stream);
    need(length > 0 && length <= 2 * 1024 * 1024, "invalid input size");
    rewind(stream);
    UInt8 *bytes = malloc((size_t)length);
    need(bytes != NULL, "cannot allocate input");
    need(fread(bytes, 1, (size_t)length, stream) == (size_t)length, "cannot read input");
    fclose(stream);
    CFDataRef data = CFDataCreate(NULL, bytes, length);
    free(bytes);
    need(data != NULL, "cannot copy input");
    return data;
}

static void save(const char *directory, const char *name, CFDataRef data) {
    char path[4096];
    int length = snprintf(path, sizeof(path), "%s/%s", directory, name);
    need(length > 0 && (size_t)length < sizeof(path), "output path too long");
    FILE *stream = fopen(path, "wb");
    need(stream != NULL, "cannot create output");
    size_t size = (size_t)CFDataGetLength(data);
    need(fwrite(CFDataGetBytePtr(data), 1, size, stream) == size, "cannot write output");
    need(fclose(stream) == 0, "cannot close output");
}

int main(int argc, char **argv) {
    need(argc >= 4, "expected profile, private output directory and Apple roots");
    CFDataRef input = load(argv[1]);
    CMSDecoderRef decoder = NULL;
    need(CMSDecoderCreate(&decoder) == errSecSuccess, "cannot create CMS decoder");
    need(CMSDecoderUpdateMessage(decoder, CFDataGetBytePtr(input), (size_t)CFDataGetLength(input)) == errSecSuccess, "invalid CMS data");
    need(CMSDecoderFinalizeMessage(decoder) == errSecSuccess, "invalid CMS container");
    size_t signers = 0;
    need(CMSDecoderGetNumSigners(decoder, &signers) == errSecSuccess && signers == 1, "exactly one CMS signer required");

    SecPolicyRef basic = SecPolicyCreateBasicX509();
    SecPolicyRef revocation = SecPolicyCreateRevocation(kSecRevocationOCSPMethod);
    need(basic != NULL && revocation != NULL, "cannot create trust policies");
    const void *policy_values[] = {basic, revocation};
    CFArrayRef policies = CFArrayCreate(NULL, policy_values, 2, &kCFTypeArrayCallBacks);
    CMSSignerStatus status = kCMSSignerUnsigned;
    SecTrustRef trust = NULL;
    need(CMSDecoderCopySignerStatus(decoder, 0, policies, false, &status, &trust, NULL) == errSecSuccess && status == kCMSSignerValid && trust != NULL, "CMS signature is invalid");

    CFMutableArrayRef anchors = CFArrayCreateMutable(NULL, 0, &kCFTypeArrayCallBacks);
    for (int index = 3; index < argc; index++) {
        CFDataRef encoded = load(argv[index]);
        SecCertificateRef anchor = SecCertificateCreateWithData(NULL, encoded);
        need(anchor != NULL, "invalid Apple root certificate");
        CFArrayAppendValue(anchors, anchor);
        CFRelease(anchor);
        CFRelease(encoded);
    }
    need(SecTrustSetAnchorCertificates(trust, anchors) == errSecSuccess, "cannot set Apple anchors");
    need(SecTrustSetAnchorCertificatesOnly(trust, true) == errSecSuccess, "cannot restrict trust anchors");
    need(SecTrustSetNetworkFetchAllowed(trust, true) == errSecSuccess, "cannot enable revocation lookup");
    CFErrorRef error = NULL;
    need(SecTrustEvaluateWithError(trust, &error), "CMS signer chain is not currently trusted");

    CFArrayRef chain = SecTrustCopyCertificateChain(trust);
    need(chain != NULL && CFArrayGetCount(chain) == 3, "expected Apple profile signing chain");
    need(CFArrayContainsValue(anchors, CFRangeMake(0, CFArrayGetCount(anchors)), CFArrayGetValueAtIndex(chain, 2)), "profile chain does not end at an explicit system Apple root");
    SecCertificateRef leaf = (SecCertificateRef)CFArrayGetValueAtIndex(chain, 0);
    SecCertificateRef issuer = (SecCertificateRef)CFArrayGetValueAtIndex(chain, 1);
    SecCertificateRef signer = NULL;
    need(CMSDecoderCopySignerCert(decoder, 0, &signer) == errSecSuccess && CFEqual(leaf, signer), "authenticated signer differs from chain leaf");
    CFStringRef common_name = NULL;
    need(SecCertificateCopyCommonName(leaf, &common_name) == errSecSuccess && CFEqual(common_name, CFSTR("Mac OS X Provisioning Profile Signing")), "CMS signer is not the macOS profile authority");

    CFDataRef content = NULL;
    need(CMSDecoderCopyContent(decoder, &content) == errSecSuccess && content != NULL, "profile has no signed content");
    CFDataRef leaf_der = SecCertificateCopyData(leaf);
    CFDataRef issuer_der = SecCertificateCopyData(issuer);
    save(argv[2], "signer.der", leaf_der);
    save(argv[2], "issuer.der", issuer_der);
    save(argv[2], "payload.plist", content);

    CFRelease(issuer_der);
    CFRelease(leaf_der);
    CFRelease(content);
    CFRelease(common_name);
    CFRelease(signer);
    CFRelease(chain);
    CFRelease(anchors);
    CFRelease(trust);
    CFRelease(policies);
    CFRelease(revocation);
    CFRelease(basic);
    CFRelease(decoder);
    CFRelease(input);
    puts("Authenticated macOS provisioning CMS signature and Apple root chain; OCSP policy enabled");
    return 0;
}
