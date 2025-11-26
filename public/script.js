// --- Firebase Configuration ---
// --- NEW Firebase Configuration (replace the old one) ---
const firebaseConfig = {
    apiKey: "AIzaSyAvuTedi4hNLDTHwNt3tElmZZmwmxBC_zo",
    authDomain: "rupeedesk7.firebaseapp.com",
    projectId: "rupeedesk7",
    storageBucket: "rupeedesk7.firebasestorage.app",
    messagingSenderId: "1013963357851",
    appId: "1:1013963357851:android:eea4e2e566c2244aed503e",
    // measurementId is optional for web – you can omit it if you don’t use Analytics
    // measurementId: "G-XXXXXXXXXX"
};

// --- Firebase Imports ---
import { initializeApp } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-app.js";
import { getAuth, onAuthStateChanged, signOut } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-auth.js";
import { getFirestore, doc, getDoc, setDoc, onSnapshot, updateDoc, increment, serverTimestamp, collection, getDocs, query, where, runTransaction, writeBatch, orderBy, limit, addDoc } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-firestore.js";

// --- App Initialization ---
const app = initializeApp(firebaseConfig);
const auth = getAuth(app);
const db = getFirestore(app);

let currentUser = null;
let userData = null;

// --- DOM Elements ---
const pages = document.querySelectorAll('.page-content');
const navItems = document.querySelectorAll('.nav-item');
const coinBalanceEl = document.getElementById('coin-balance');
const referralCodeEl = document.getElementById('referral-code');
const themeCheckbox = document.getElementById('theme-checkbox');

// --- Helper Functions ---
function getDeviceId() { let deviceId = localStorage.getItem('deviceGuid'); if (!deviceId) { deviceId = crypto.randomUUID(); localStorage.setItem('deviceGuid', deviceId); } return deviceId; }
function showModal(title, body, actions = '<button class="modal-button-primary" onclick="closeModal()">OK</button>') { const modal = document.getElementById('modal'); modal.querySelector('#modal-title').innerHTML = title; modal.querySelector('#modal-body').innerHTML = body; const actionsDiv = modal.querySelector('.modal-actions') || document.createElement('div'); if (!actionsDiv.className) { actionsDiv.className = 'modal-actions'; modal.querySelector('.modal-content').appendChild(actionsDiv); } actionsDiv.innerHTML = actions; modal.style.display = 'flex'; }
window.closeModal = function() { document.getElementById('modal').style.display = 'none'; }
function handleError(error) { console.error("An error occurred:", error); showModal("Error", `<p>${error.message}</p>`); }

// --- Core App Logic ---
function updateUI() {
    if (!userData) return;
    coinBalanceEl.textContent = `₹${(userData.balance || 0).toFixed(2)}`;
    referralCodeEl.textContent = userData.referralCode;
    const waBtn = document.getElementById('whatsapp-bind-btn');
    waBtn.textContent = userData.whatsAppNumber ? 'Bound' : 'Bind Now';
    waBtn.disabled = !!userData.whatsAppNumber;
    document.getElementById('sms-count').textContent = (userData.smsTask && userData.smsTask.count) ? userData.smsTask.count : 0;
    document.getElementById('profile-custom-id').textContent = userData.customId || '...';
    const profileStatusEl = document.getElementById('profile-status');
    profileStatusEl.textContent = userData.status;
    profileStatusEl.className = `status-${userData.status}`;
    document.getElementById('user-id-display').textContent = userData.uid.substring(0,15) + '...';
    document.getElementById('device-id-display').textContent = (userData.deviceGuid || '').substring(0, 15) + '...';
}

async function setupUser(user) {
    const userRef = doc(db, "users", user.uid);
    try {
        const userSnap = await getDoc(userRef);
        if (!userSnap.exists()) {
            const referralCode = Math.random().toString(36).substring(2, 8).toUpperCase();
            const customId = `RUPE${Math.floor(1000 + Math.random() * 9000)}`;
            const newUserData = { uid: user.uid, customId, email: user.email, balance: 10.00, referralCode, deviceGuid: getDeviceId(), status: "active", bankAccount: null, whatsAppNumber: null, dailyCheckin: { lastClaimed: null }, dailySpin: { lastSpin: null }, smsTask: { count: 0 }, createdAt: serverTimestamp(), referrerId: null };
            const enteredReferralCode = sessionStorage.getItem('referralCode');
            if (enteredReferralCode) {
                const referrerQuery = query(collection(db, "users"), where("referralCode", "==", enteredReferralCode));
                const referrerSnap = await getDocs(referrerQuery);
                if (!referrerSnap.empty) {
                    const referrerDoc = referrerSnap.docs[0];
                    newUserData.referrerId = referrerDoc.id;
                    await addDoc(collection(db, "referrals"), { referrerId: referrerDoc.id, refereeId: user.uid, rewarded: false, totalCommissionEarned: 0, createdAt: serverTimestamp() });
                }
                sessionStorage.removeItem('referralCode');
            }
            await setDoc(userRef, newUserData);
            showModal("Welcome!", "<p>You've received a welcome bonus of ₹10.00!</p>");
        } else { if (userSnap.data().status === 'banned') document.body.innerHTML = '<h1>Your account has been suspended.</h1>'; }
    } catch (error) { handleError({ message: "Could not setup your profile." }); }
}

function listenToUserData(uid) {
    const userRef = doc(db, "users", uid);
    onSnapshot(userRef, (docSnap) => {
        if (docSnap.exists()) { userData = docSnap.data(); updateUI(); }
    }, (error) => { handleError({ message: "Permission Denied. Could not load profile." }); });
}

async function fetchLatestAnnouncement() {
    try {
        const q = query(collection(db, "announcements"), orderBy("createdAt", "desc"), limit(1));
        const querySnapshot = await getDocs(q);
        if (!querySnapshot.empty) {
            const announcement = querySnapshot.docs[0].data();
            document.getElementById('announcement-title').textContent = announcement.title;
            document.getElementById('announcement-body').textContent = announcement.content;
            document.getElementById('announcement-banner').classList.remove('hidden');
        }
    } catch (error) { console.error("Could not fetch announcement:", error); }
}

async function loadReferralStats() {
    if (!currentUser) return;
    const totalReferralsEl = document.getElementById('total-referrals');
    const totalEarningsEl = document.getElementById('total-referral-earnings');
    const referralListEl = document.getElementById('referral-list');
    totalReferralsEl.textContent = '...'; totalEarningsEl.textContent = '...'; referralListEl.innerHTML = '<li>Loading...</li>';
    try {
        const q = query(collection(db, "referrals"), where("referrerId", "==", currentUser.uid));
        const referralSnap = await getDocs(q);
        const referrals = referralSnap.docs.map(doc => doc.data());
        totalReferralsEl.textContent = referrals.length;
        let totalEarnings = 0;
        referrals.forEach(ref => { totalEarnings += ref.totalCommissionEarned || 0; });
        totalEarningsEl.textContent = `₹${totalEarnings.toFixed(2)}`;
        if (referrals.length === 0) { referralListEl.innerHTML = '<li>You haven\'t referred anyone yet.</li>'; return; }
        const refereeIds = referrals.map(ref => ref.refereeId);
        if(refereeIds.length === 0) return;
        const usersQuery = query(collection(db, "users"), where("uid", "in", refereeIds));
        const userSnap = await getDocs(usersQuery);
        const userMap = new Map();
        userSnap.forEach(doc => { userMap.set(doc.data().uid, doc.data()); });
        referralListEl.innerHTML = '';
        referrals.forEach(ref => {
            const refereeData = userMap.get(ref.refereeId);
            if (refereeData) {
                const listItem = document.createElement('li');
                listItem.className = 'referral-list-item';
                const commission = (ref.totalCommissionEarned || 0).toFixed(2);
                listItem.innerHTML = `<p>${refereeData.customId}</p><span>Earned: ₹${commission}</span>`;
                referralListEl.appendChild(listItem);
            }
        });
    } catch (error) { console.error("Error loading referral stats:", error); referralListEl.innerHTML = '<li>Could not load referrals.</li>'; }
}

function handleNavigation(selector, targetAttribute) {
    document.querySelectorAll(selector).forEach(item => {
        item.addEventListener('click', (e) => {
            e.preventDefault();
            const pageId = item.dataset[targetAttribute];
            if (selector === '.nav-item') { navItems.forEach(nav => nav.classList.remove('active')); item.classList.add('active'); }
            pages.forEach(page => { page.classList.toggle('hidden', page.id !== pageId); page.classList.toggle('active', page.id === pageId); });
            if (pageId === 'referral-page') { loadReferralStats(); }
        });
    });
}
handleNavigation('.nav-item', 'page');
handleNavigation('.action-item[data-page]', 'page');
handleNavigation('.back-btn', 'page');

onAuthStateChanged(auth, async (user) => { if (user) { currentUser = user; await setupUser(user); listenToUserData(user.uid); fetchLatestAnnouncement(); } else { window.location.href = 'login.html'; } });
document.getElementById('close-announcement-btn').addEventListener('click', () => { document.getElementById('announcement-banner').classList.add('hidden'); });
themeCheckbox.addEventListener('change', () => { const newTheme = themeCheckbox.checked ? 'dark' : 'light'; localStorage.setItem('theme', newTheme); document.body.classList.toggle('dark', newTheme === 'dark'); });
const savedTheme = localStorage.getItem('theme') || 'light';
document.body.classList.toggle('dark', savedTheme === 'dark');
themeCheckbox.checked = (savedTheme === 'dark');

const dailyCheckin = async () => { if (!userData || !currentUser) return; const today = new Date().toDateString(); const lastClaim = userData.dailyCheckin.lastClaimed ? new Date(userData.dailyCheckin.lastClaimed.seconds * 1000).toDateString() : null; if (today === lastClaim) { return showModal("Already Claimed", "<p>You have already claimed your check-in bonus today.</p>"); } try { await updateDoc(doc(db, "users", currentUser.uid), { balance: increment(1.00), 'dailyCheckin.lastClaimed': serverTimestamp() }); showModal("Success", `<p>You've earned ₹1.00 from your daily check-in!</p>`); } catch (error) { handleError(error); } };
document.getElementById('copy-referral-btn').addEventListener('click', () => { if(userData && userData.referralCode) { navigator.clipboard.writeText(userData.referralCode).then(() => showModal("Copied!", "<p>Referral code copied to clipboard.</p>")).catch(err => handleError({ message: 'Failed to copy.' })); } });
document.getElementById('daily-spin-btn').addEventListener('click', () => { if (!userData || !currentUser) return; const today = new Date().toDateString(); const lastSpin = userData.dailySpin.lastSpin ? new Date(userData.dailySpin.lastSpin.seconds * 1000).toDateString() : null; if (today === lastSpin) { return showModal("Already Spin", "<p>You have already used your daily spin. Come back tomorrow!</p>"); } const segments = [ { value: 5, label: '₹5' }, { value: 0, label: 'Try Again' }, { value: 10, label: '₹10' }, { value: 0, label: 'Try Again' }, { value: 200, label: '₹200' }, { value: 0, label: 'Try Again' }, { value: 2, label: '₹2' }, { value: 0, label: 'Try Again' }]; let svg = `<svg id="wheel" width="250" height="250" viewBox="0 0 100 100">`; const angle = 360 / segments.length; const colors = ['#f87171', '#fbbf24', '#34d399', '#60a5fa', '#c084fc', '#f472b6', '#a3e635', '#fde047']; segments.forEach((seg, i) => { const [x, y] = [50 + 50 * Math.cos(Math.PI / 180 * (angle * i)), 50 + 50 * Math.sin(Math.PI / 180 * (angle * i))]; svg += `<path d="M50 50 L${x} ${y} A50 50 0 0 1 ${50 + 50 * Math.cos(Math.PI / 180 * (angle * (i + 1)))} ${50 + 50 * Math.sin(Math.PI / 180 * (angle * (i + 1)))} Z" fill="${colors[i % colors.length]}"></path>`; const textAngle = angle * i + angle / 2; const [tx, ty] = [50 + 35 * Math.cos(Math.PI / 180 * textAngle), 50 + 35 * Math.sin(Math.PI / 180 * textAngle)]; svg += `<text x="${tx}" y="${ty}" transform="rotate(${textAngle + 90} ${tx} ${ty})" fill="white" text-anchor="middle" font-size="6" font-weight="bold">${seg.label}</text>`; }); svg += `</svg>`; const body = `<div id="spin-container"><div id="spin-marker"></div>${svg}</div>`; const actions = `<button class="modal-button-primary" id="spin-it-btn">Spin Now!</button>`; showModal("Daily Spin", body, actions); document.getElementById('spin-it-btn').addEventListener('click', async () => { document.getElementById('spin-it-btn').disabled = true; const p = Math.random(); let resultIndex; if (p < 0.6) resultIndex = Math.random() < 0.5 ? 1 : 3; else if (p < 0.95) resultIndex = 0; else if (p < 0.99) resultIndex = 2; else resultIndex = 4; const reward = segments[resultIndex].value; const totalRotations = 5 * 360; const targetAngle = -((angle * resultIndex) + (angle / 2) - (angle * 0.25) + (Math.random() * angle * 0.5)); const finalRotation = totalRotations + targetAngle; document.getElementById('wheel').style.transform = `rotate(${finalRotation}deg)`; setTimeout(async () => { try { await updateDoc(doc(db, "users", currentUser.uid), { balance: increment(reward), 'dailySpin.lastSpin': serverTimestamp() }); showModal("Congratulations!", `<p>You won ₹${reward.toFixed(2)}!</p>`); } catch (error) { handleError(error); } }, 5500); }); });
// ==========================================
//  STRICT BIND LOGIC (With Verification)
// ==========================================
document.getElementById('whatsapp-bind-btn').addEventListener('click', () => {
    if (!currentUser) return;
    
    const body = `
        <p>Enter your WhatsApp Number (e.g., 919876543210)</p>
        <input type="tel" id="whatsapp-input" placeholder="919876543210" maxlength="13">
        <p style="font-size:12px; color:gray; margin-top:5px;">Must include country code (e.g., 91)</p>
    `;
    const actions = `<button class="modal-button-secondary" onclick="closeModal()">Cancel</button><button class="modal-button-primary" id="submit-whatsapp">Get Code</button>`;
    
    showModal("Bind Device", body, actions);
    
    document.getElementById('submit-whatsapp').addEventListener('click', async () => {
        const btn = document.getElementById('submit-whatsapp');
        const number = document.getElementById('whatsapp-input').value.trim();
        
        if (number.length < 10) return alert("Invalid Number");
        
        btn.innerHTML = "Generating...";
        btn.disabled = true;
        
        try {
            // 1. Request Pairing Code
            const res = await fetch('/api/earning/add-device', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    userId: currentUser.uid,
                    customId: userData.customId,
                    phoneNumber: number
                })
            });
            const data = await res.json();
            
            if (data.success && data.pairingCode) {
                // 2. Show Pairing Code UI
                const pairBody = `
                    <div style="text-align:center;">
                        <p>Open WhatsApp > Linked Devices > Link with phone number</p>
                        <h2 style="font-size:32px; letter-spacing:5px; color:#f97316; margin:15px 0; font-weight:bold;">${data.pairingCode}</h2>
                        <p style="color:gray;">Enter this code in WhatsApp immediately.</p>
                        <div id="connection-status" style="margin-top:15px; font-weight:bold; color:#f97316; font-size: 1.1rem;">
                            <i class="fas fa-spinner fa-spin"></i> Waiting for connection...
                        </div>
                    </div>
                `;
                // We remove the "I Connected" button because we will auto-detect
                showModal("Pairing Code", pairBody, `<button class="modal-button-secondary" onclick="closeModal()">Cancel</button>`);
                
                // 3. START STRICT POLLING (Check if actually connected)
                let attempts = 0;
                const pollInterval = setInterval(async () => {
                    attempts++;
                    
                    // Timeout after 2 minutes (40 attempts * 3 seconds)
                    if (attempts > 40) {
                        clearInterval(pollInterval);
                        const statusEl = document.getElementById('connection-status');
                        if (statusEl) {
                            statusEl.innerHTML = '<span style="color:red">❌ Timeout. Pairing failed.</span>';
                        }
                        return;
                    }
                    
                    try {
                        // Check Backend Status
                        const statusRes = await fetch('/api/whatsapp/status');
                        const statusData = await statusRes.json();
                        
                        // Find our specific device
                        const device = statusData.devices.find(d => d.phoneNumber === number || d.id === number);
                        
                        // 4. IF CONNECTED -> Update Firebase & UI
                        if (device && device.connected === true) {
                            clearInterval(pollInterval);
                            
                            const statusEl = document.getElementById('connection-status');
                            if (statusEl) {
                                statusEl.innerHTML = '<span style="color:#10B981">✅ Connected Successfully!</span>';
                            }
                            
                            // NOW we save to Firebase because we verified it
                            await updateDoc(doc(db, "users", currentUser.uid), { whatsAppNumber: number });
                            
                            setTimeout(() => {
                                closeModal();
                                location.reload(); // Reload to update UI state
                            }, 2000);
                        }
                    } catch (err) {
                        console.log("Polling...", err);
                    }
                }, 3000); // Check every 3 seconds
                
            } else {
                alert("Error: " + (data.error || "Failed to generate code"));
                closeModal();
            }
        } catch (e) {
            console.error(e);
            alert("Connection Error to Backend");
            closeModal();
        }
    });
});

document.getElementById('logout-btn').addEventListener('click', () => { signOut(auth); });

// Swiper Init
document.addEventListener('DOMContentLoaded', () => {
    new Swiper(".mySwiper", { loop: true, autoplay: { delay: 3500 }, pagination: { el: ".swiper-pagination", clickable: true } });
});

document.getElementById('assign-sms-btn').addEventListener('click', async () => {
    if (!userData || !currentUser) return;
    let currentSmsCount = (userData.smsTask && userData.smsTask.count) ? userData.smsTask.count : 0;
    if (currentSmsCount >= 100) { return showModal("Limit Reached", "<p>You have completed the maximum number of SMS tasks for today.</p>"); }
    const BATCH_SIZE = 5; let tasksCompletedInBatch = 0;
    try {
        const q = query(collection(db, "smsInventory"), where("assigned", "==", false));
        const querySnapshot = await getDocs(q);
        if (querySnapshot.empty) { return showModal("No Tasks", "<p>Sorry, there are no SMS tasks available right now.</p>"); }
        const smsBatch = querySnapshot.docs.slice(0, BATCH_SIZE);
        async function processSmsTask(taskIndex) {
            currentSmsCount = (userData.smsTask && userData.smsTask.count) ? userData.smsTask.count : 0;
            if (taskIndex >= smsBatch.length || currentSmsCount >= 100) { if (tasksCompletedInBatch > 0) { return showModal("Batch Complete!", `<p>You sent ${tasksCompletedInBatch} messages!</p>`); } return; }
            const smsDoc = smsBatch[taskIndex];
            const smsData = smsDoc.data();
            await updateDoc(doc(db, "smsInventory", smsDoc.id), { assigned: true });
            const smsLink = `sms:${smsData.number}?body=${encodeURIComponent(smsData.message)}`;
            const body = `<p class="text-lg font-bold">Task ${tasksCompletedInBatch + 1}</p><p>Click below to open your SMS app.</p><br><a href="${smsLink}" class="modal-button-primary" style="display: block; text-align: center;">Open SMS App</a>`;
            const actions = `<button class="modal-button-secondary" onclick="closeModal()">Cancel</button><button class="modal-button-primary" id="claim-and-next-btn">I Sent It, Get Next</button>`;
            showModal("SMS Batch Task", body, actions);
            document.getElementById('claim-and-next-btn').addEventListener('click', async () => {
                try {
                    const batch = writeBatch(db);
                    const taskReward = 0.17;
                    const userRef = doc(db, "users", currentUser.uid);
                    batch.update(userRef, { balance: increment(taskReward), 'smsTask.count': increment(1) });
                    if (userData.referrerId) {
                        const commissionRate = 0.10;
                        const commissionAmount = taskReward * commissionRate;
                        const referrerRef = doc(db, "users", userData.referrerId);
                        batch.update(referrerRef, { balance: increment(commissionAmount) });
                        const referralQuery = query(collection(db, "referrals"), where("refereeId", "==", currentUser.uid), where("referrerId", "==", userData.referrerId));
                        const referralSnap = await getDocs(referralQuery);
                        if (!referralSnap.empty) {
                            const referralDocRef = referralSnap.docs[0].ref;
                            batch.update(referralDocRef, { totalCommissionEarned: increment(commissionAmount) });
                        }
                    }
                    const inventoryRef = doc(db, "smsInventory", smsDoc.id);
                    batch.delete(inventoryRef);
                    await batch.commit();
                    tasksCompletedInBatch++;
                    processSmsTask(taskIndex + 1);
                } catch (error) { handleError(error); }
            }, { once: true });
        }
        processSmsTask(0);
    } catch (error) { handleError(error); }
});

document.getElementById('bank-account-btn').addEventListener('click', () => { if (!userData || !currentUser) return; if (userData.bankAccount) { const { holderName, accountNumber, ifscCode } = userData.bankAccount; const body = `<div class="account-details-view"><p><strong>Holder Name:</strong> ${holderName}</p><p><strong>Account Number:</strong> ****${accountNumber.slice(-4)}</p><p><strong>IFSC Code:</strong> ${ifscCode}</p></div>`; showModal("Bank Account Details", body); } else { const body = `<p>Please enter your bank details.</p><input type="text" id="holder-name-input" placeholder="Account Holder Name"><input type="text" id="account-number-input" placeholder="Bank Account Number"><input type="text" id="ifsc-code-input" placeholder="IFSC Code">`; const actions = `<button class="modal-button-secondary" onclick="closeModal()">Cancel</button><button class="modal-button-primary" id="submit-bank-details">Submit</button>`; showModal("Bind Bank Account", body, actions); document.getElementById('submit-bank-details').addEventListener('click', async () => { const holderName = document.getElementById('holder-name-input').value.trim(); const accountNumber = document.getElementById('account-number-input').value.trim(); const ifscCode = document.getElementById('ifsc-code-input').value.trim().toUpperCase(); if (!holderName || !accountNumber || !ifscCode) return alert("Please fill in all fields."); try { await updateDoc(doc(db, "users", currentUser.uid), { bankAccount: { holderName, accountNumber, ifscCode } }); showModal("Success", "<p>Your bank account has been linked successfully!</p>"); } catch (error) { handleError(error); } }); } });
document.getElementById('withdraw-btn').addEventListener('click', () => { if (!userData || !currentUser) return; if (!userData.bankAccount) return showModal("No Bank Account", "<p>Please add your bank account before making a withdrawal.</p>"); const MIN_WITHDRAWAL = 50; if (userData.balance < MIN_WITHDRAWAL) return showModal("Insufficient Balance", `<p>You need at least ₹${MIN_WITHDRAWAL} to withdraw.</p>`); const body = `<p>Balance: <strong>₹${userData.balance.toFixed(2)}</strong>.</p><input type="number" id="withdraw-amount-input" placeholder="Enter amount (min ₹${MIN_WITHDRAWAL})">`; const actions = `<button class="modal-button-secondary" onclick="closeModal()">Cancel</button><button class="modal-button-primary" id="submit-withdrawal">Request</button>`; showModal("Request Withdrawal", body, actions); document.getElementById('submit-withdrawal').addEventListener('click', async () => { const amount = parseFloat(document.getElementById('withdraw-amount-input').value); if (isNaN(amount) || amount < MIN_WITHDRAWAL || amount > userData.balance) return alert("Please enter a valid amount."); try { const userRef = doc(db, "users", currentUser.uid); const withdrawalRef = doc(collection(db, "withdrawals")); await runTransaction(db, async (transaction) => { const userDoc = await transaction.get(userRef); if (!userDoc.exists()) throw "User not found!"; const newBalance = userDoc.data().balance - amount; if (newBalance < 0) throw "Insufficient funds!"; transaction.update(userRef, { balance: newBalance }); transaction.set(withdrawalRef, { userId: currentUser.uid, customId: userData.customId, amount, status: 'pending', bankDetails: userData.bankAccount, requestedAt: serverTimestamp() }); }); showModal("Success", `<p>Your withdrawal request for ₹${amount.toFixed(2)} has been submitted.</p>`); } catch (error) { handleError(error); } }); });
document.getElementById('withdrawal-history-btn').addEventListener('click', async () => { if (!currentUser) return; try { const q = query(collection(db, "withdrawals"), where("userId", "==", currentUser.uid), orderBy("requestedAt", "desc")); const querySnapshot = await getDocs(q); let body = '<div class="history-list">'; if (querySnapshot.empty) { body += '<p class="text-center">You have no withdrawal history.</p>'; } else { querySnapshot.forEach(doc => { const data = doc.data(); const date = data.requestedAt ? new Date(data.requestedAt.seconds * 1000).toLocaleString('en-IN') : 'N/A'; body += `<div class="history-item"><div class="history-info"><strong>₹${data.amount.toFixed(2)}</strong><span class="history-date">${date}</span></div><span class="history-status status-${data.status}">${data.status}</span></div>`; }); } body += '</div>'; showModal("Withdrawal History", body); } catch (error) { handleError(error); } });
document.getElementById('logout-btn').addEventListener('click', () => { signOut(auth).catch(handleError); });

// --- Swiper Initialization ---
document.addEventListener('DOMContentLoaded', () => {
    const swiper = new Swiper(".mySwiper", { loop: true, autoplay: { delay: 3500, disableOnInteraction: false, }, pagination: { el: ".swiper-pagination", clickable: true, }, effect: "creative", creativeEffect: { prev: { shadow: true, translate: [0, 0, -400], }, next: { translate: ["100%", 0, 0], }, }, });
    const checkinSlide = document.querySelector('.daily-checkin-slide');
    if (checkinSlide) {
        checkinSlide.addEventListener('click', dailyCheckin);
    }
});


