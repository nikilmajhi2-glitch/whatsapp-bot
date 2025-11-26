// --- Firebase Configuration ---
const firebaseConfig = {
    apiKey: "AIzaSyAvuTedi4hNLDTHwNt3tElmZZmwmxBC_zo",
    authDomain: "rupeedesk7.firebaseapp.com",
    projectId: "rupeedesk7",
    storageBucket: "rupeedesk7.firebasestorage.app",
    messagingSenderId: "1013963357851",
    appId: "1:1013963357851:android:eea4e2e566c2244aed503e",
};

// --- Imports ---
import { initializeApp } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-app.js";
import { getAuth, onAuthStateChanged, signOut } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-auth.js";
import { getFirestore, doc, getDoc, setDoc, onSnapshot, updateDoc, increment, serverTimestamp, collection, getDocs, query, where, runTransaction, writeBatch, orderBy, limit, addDoc } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-firestore.js";

const app = initializeApp(firebaseConfig);
const auth = getAuth(app);
const db = getFirestore(app);

let currentUser = null;
let userData = null;

// --- Elements ---
const pages = document.querySelectorAll('.page-content');
const navItems = document.querySelectorAll('.nav-item');
const coinBalanceEl = document.getElementById('coin-balance');
const referralCodeEl = document.getElementById('referral-code');
const themeCheckbox = document.getElementById('theme-checkbox');

// --- Helper Functions ---
function getDeviceId() { let deviceId = localStorage.getItem('deviceGuid'); if (!deviceId) { deviceId = crypto.randomUUID(); localStorage.setItem('deviceGuid', deviceId); } return deviceId; }
function showModal(title, body, actions = '<button class="modal-button-primary" onclick="closeModal()">OK</button>') { const modal = document.getElementById('modal'); modal.querySelector('#modal-title').innerHTML = title; modal.querySelector('#modal-body').innerHTML = body; const actionsDiv = modal.querySelector('.modal-actions') || document.createElement('div'); if (!actionsDiv.className) { actionsDiv.className = 'modal-actions'; modal.querySelector('.modal-content').appendChild(actionsDiv); } actionsDiv.innerHTML = actions; modal.style.display = 'flex'; }
window.closeModal = function() { document.getElementById('modal').style.display = 'none'; }
function handleError(error) { console.error("Error:", error); showModal("Error", `<p>${error.message}</p>`); }

// --- UI Updates ---
function updateUI() {
    if (!userData) return;
    coinBalanceEl.textContent = `₹${(userData.balance || 0).toFixed(2)}`;
    referralCodeEl.textContent = userData.referralCode;
    const waBtn = document.getElementById('whatsapp-bind-btn');
    // We check locally if they are bound via Firestore, OR via the Go status
    waBtn.textContent = userData.whatsAppNumber ? 'Connected' : 'Bind Now';
    waBtn.disabled = !!userData.whatsAppNumber;
    
    document.getElementById('profile-custom-id').textContent = userData.customId || '...';
    document.getElementById('user-id-display').textContent = userData.uid.substring(0,15) + '...';
}

async function setupUser(user) {
    const userRef = doc(db, "users", user.uid);
    const userSnap = await getDoc(userRef);
    if (!userSnap.exists()) {
        const referralCode = Math.random().toString(36).substring(2, 8).toUpperCase();
        const customId = `RUPE${Math.floor(1000 + Math.random() * 9000)}`;
        const newUserData = { uid: user.uid, customId, email: user.email, balance: 10.00, referralCode, deviceGuid: getDeviceId(), status: "active", whatsAppNumber: null, createdAt: serverTimestamp() };
        await setDoc(userRef, newUserData);
    }
}

function listenToUserData(uid) {
    onSnapshot(doc(db, "users", uid), (docSnap) => {
        if (docSnap.exists()) { userData = docSnap.data(); updateUI(); }
    });
}

// --- Navigation ---
document.querySelectorAll('.nav-item').forEach(item => {
    item.addEventListener('click', (e) => {
        e.preventDefault();
        const pageId = item.dataset.page;
        navItems.forEach(nav => nav.classList.remove('active'));
        item.classList.add('active');
        pages.forEach(page => { page.classList.toggle('hidden', page.id !== pageId); page.classList.toggle('active', page.id === pageId); });
    });
});

// --- Auth State ---
onAuthStateChanged(auth, async (user) => { 
    if (user) { 
        currentUser = user; 
        await setupUser(user); 
        listenToUserData(user.uid); 
    } else { 
        window.location.href = 'login.html'; 
    } 
});

// ==========================================
//  NEW WHATSAPP BIND LOGIC (Connects to Go)
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
            // CALL GO BACKEND
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
                // Show Pairing Code
                const pairBody = `
                    <div style="text-align:center;">
                        <p>Open WhatsApp > Linked Devices > Link with phone number</p>
                        <h2 style="font-size:32px; letter-spacing:5px; color:#f97316; margin:15px 0; font-weight:bold;">${data.pairingCode}</h2>
                        <p style="color:gray;">Enter this code in WhatsApp immediately.</p>
                        <div id="connection-status" style="margin-top:10px; font-weight:bold; color:#f97316;">Waiting for connection...</div>
                    </div>
                `;
                showModal("Pairing Code", pairBody, `<button class="modal-button-primary" onclick="location.reload()">I Connected It</button>`);

                // Update Firestore so UI knows we attempted binding
                await updateDoc(doc(db, "users", currentUser.uid), { whatsAppNumber: number });
                
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
